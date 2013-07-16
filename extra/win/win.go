package main

import (
	"os"
	"os/signal"
	"log"
	"strings"
	"bufio"
	"io"
	"io/ioutil"
	"os/exec"
	"fmt"
	"time"
	"syscall"
	"strconv"
	"runtime"
	"github.com/kr/pty"
)

var debug = false
var prevInput = ""

func Must(err error) {
	if err != nil {
		if !debug {
			_, file, line, _ := runtime.Caller(2)
			log.Fatalf("%s:%d: %s", file, line, err.Error())
		} else {
			i := 1
			fmt.Println("Error" + err.Error() + " at:")
			for {
				_, file, line, ok := runtime.Caller(i)
				if !ok {
					break
				}
				fmt.Printf("\t %s:%d\n", file, line)
				i++
			}
		}
	}
}

func read(fd *os.File) string {
	b := make([]byte, 1024)
	n, err := fd.Read(b)
	Must(err)
	return string(b[:n])
}

const (
	ANSI_NORMAL = iota
	ANSI_0D
	ANSI_ESCAPE
)

func updateDir(cmd *exec.Cmd, ctlfd io.ReadWriter) {
	dest, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", cmd.Process.Pid))
	if err != nil {
		return
	}
	ctlfd.Write([]byte(fmt.Sprintf("name %s/+Win\n", dest)))
}

func outputReader(cmd *exec.Cmd, stdout io.Reader, bodyfd *os.File, outbufid string, ctlfd *os.File, addrfd *os.File, xdatafd *os.File) {
	bufout := bufio.NewReader(stdout)
	bufbody := bufio.NewWriter(bodyfd)
	escseq := []byte{}
	state := ANSI_NORMAL
	for {
		if bufout.Buffered() == 0 {
			if debug {
				log.Println("flushing1")
			}
			bufbody.Flush()
			updateDir(cmd, ctlfd)
		}
		ch, err := bufout.ReadByte()
		if err != nil {
			if debug {
				fmt.Println("Exit output reader with error: " + err.Error())
			}
			bufbody.Flush()
			return
		}

		switch state {
		case ANSI_NORMAL:
			switch ch {
			case 0x0d:
				state = ANSI_0D
			case 0x08:
				bufbody.Flush()
				_, err = addrfd.Write([]byte("-#1"))
				Must(err)
				xdatafd.Write([]byte{ 0 })
			case 0x1b:
				escseq = []byte{}
				state = ANSI_ESCAPE
			default:
				Must(bufbody.WriteByte(ch))
				if ch == '\n' {
					if debug {
						log.Println("flushing2")
					}
					bufbody.Flush()
					updateDir(cmd, ctlfd)
				}
			}
		case ANSI_ESCAPE:
			escseq = append(escseq, ch)
			if (len(escseq) > 1) && (ch >= 0x40) && (ch <= 0x7e) {
				state = ANSI_NORMAL
				switch escseq[len(escseq)-1] {
				case 'J':
					if debug {
						fmt.Println("Requesting screen clear")
					}
					bufbody.Flush()
					_, err = addrfd.Write([]byte(","))
					Must(err)
					xdatafd.Write([]byte{ 0 })
				case 'H':
					if debug {
						fmt.Println("Requesting back to home")
					}
					bufbody.Flush()
					// XXX Clears a race with the processing of the event generated by the flush -- very wonky
					time.Sleep(500 * time.Millisecond)
					addr := readAddr(outbufid)
					_, err = addrfd.Write([]byte("#0"))
					Must(err)
					ctlfd.Write([]byte("dot=addr\n"))
					Must(err)
					fmt.Fprintf(addrfd, "#%d,#%d", addr[0], addr[1])
				}
			}

		case ANSI_0D:
			state = ANSI_NORMAL
			switch ch {
			case 0x0a:
				Must(bufbody.WriteByte(ch))
				bufbody.Flush()
			default:
				if debug {
					fmt.Println("Requesting line delete")
				}
				bufbody.Flush()
				_, err = addrfd.Write([]byte("-+"))
				Must(err)
				xdatafd.Write([]byte{ 0 })
				Must(bufbody.WriteByte(ch))
			}
		}
	}

	if debug {
		fmt.Println("output reader finished")
	}

	bufbody.Flush()
}

func readAddr(outbufid string) []int {
	addrfd, err := os.Open(os.ExpandEnv("$yd/" + outbufid + "/addr"))
	Must(err)
	defer addrfd.Close()
	str := read(addrfd)
	v := strings.Split(str, ",")
	iv := []int{ 0, 0 }
	iv[0], err = strconv.Atoi(v[0])
	Must(err)
	iv[1], err = strconv.Atoi(v[1])
	Must(err)
	return iv
}

func readXdata(outbufid string) string {
	xdatafd, err := os.Open(os.ExpandEnv("$yd/" + outbufid + "/xdata"))
	Must(err)
	defer xdatafd.Close()
	xdata, err := ioutil.ReadAll(xdatafd)
	Must(err)
	return string(xdata)
}

func eventReader(eventfd *os.File, ctlfd *os.File, addrfd *os.File, bodyfd *os.File, pty *os.File, outbufid string) {
	buf := make([]byte, 1024)
	addrfd.Write([]byte("$"))
	for {
		if debug {
			log.Println("Waiting for event")
		}
		n, err := eventfd.Read(buf)
		if err == io.EOF {
			break
		}
		Must(err)
		if n < 2 {
			log.Fatalf("Not enough read from event file")
		}

		event := string(buf[:n])

		origin := event[0]
		etype := event[1]
		v := strings.SplitN(event[2:], " ",  5)
		if len(v) != 5 {
			log.Fatalf("Wrong number of arguments from split: %d", len(v))
		}

		s, err:= strconv.Atoi(v[0])
		Must(err)
		_, err = strconv.Atoi(v[1])
		Must(err)
		flags, err := strconv.Atoi(v[2])
		Must(err)
		arglen, err := strconv.Atoi(v[3])
		Must(err)

		arg := v[4]

		for len(arg) < arglen {
			n, err := eventfd.Read(buf)
			Must(err)
			arg += string(buf[:n])
			event += string(buf[:n])
		}

		if arg[len(arg)-1] == '\n' {
			arg = arg[:len(arg)-1]
		}

		if (debug) {
			fmt.Printf("event <%s>\n", event)
		}

		switch etype {
		case 'x', 'X':
			if flags != 0 {
				_, err := eventfd.Write([]byte(event))
				Must(err)
			} else {
				switch arg {
				case "Term":
					//TODO: send sigterm
				case "Prev":
					if prevInput != "" {
						bodyfd.Write([]byte(prevInput))
						prevInput = ""
					}
				default:
					bodyfd.Write([]byte(arg))
					bodyfd.Write([]byte{ '\n' })
					prevInput = arg
					pty.Write([]byte(arg))
					pty.Write([]byte{ '\n' })
				}
			}
			
			Must(err)
		case 'l', 'L':
			_, err := eventfd.Write([]byte(event))
			Must(err)

		case 'I':
			if (origin == 'E') || (origin == 'F') {
				if debug {
					fmt.Println("Moving address forward")
				}
				_, err = addrfd.Write([]byte("$"))
				Must(err)
				_, err = ctlfd.Write([]byte("dot=addr\n"))
				Must(err)
			} else {
				addr := readAddr(outbufid)
				if (addr[0] <= s) && (len(arg) > 0) && (arg[len(arg)-1] == '\n') {
					if debug {
						fmt.Printf("From to: %d $\n", addr[0])
					}
					fmt.Fprintf(addrfd, "#%d,$", addr[0])
					command := readXdata(outbufid)
					if debug {
						fmt.Printf("Sending: %s", command)
					}
					prevInput = command
					pty.Write([]byte(command))
				} else {
					if debug {
						if addr[0] > s {
							fmt.Printf("Before input address %d %d\n", addr[0], s)
						} else {
							fmt.Printf("Not terminted by newline\n")
						}
					}
				}
			}
		}
	}
}

func run(c *exec.Cmd) *os.File {
	pty, tty, err := pty.Open()
	Must(err)
	defer tty.Close()

	termios, err := TcGetAttr(tty)
	Must(err)
	termios.SetIFlags(ICRNL|IUTF8)
	termios.SetOFlags(ONLRET)
	termios.SetCFlags(CS8|CREAD)
	termios.SetLFlags(ICANON)
	termios.SetSpeed(38400)
	err = TcSetAttr(tty, TCSANOW, termios)
	Must(err)
	err = TcSetAttr(pty, TCSANOW, termios)

	c.Stdout = tty
	c.Stdin = tty
	c.Stderr = tty
	c.SysProcAttr = &syscall.SysProcAttr{Setctty: true, Setsid: true}
	err = c.Start()
	if err != nil {
		pty.Close()
		Must(err)
	}
	return pty
}

func notifyProc(notifyChan <-chan os.Signal, endChan <-chan bool, bodyfd io.ReadWriter, ctlfd io.ReadWriter) {
	if debug {
		fmt.Println("Waiting for signal")
	}
	select {
	case <- notifyChan:
	case <- endChan:
	}
	if debug {
		fmt.Println("Ending")
	}
	bodyfd.Write([]byte("~\n"))
	ctlfd.Write([]byte("dump\n"))
	ctlfd.Write([]byte("dumpdir\n"))
	os.Exit(0)
}

func findWin() (string, *os.File, *os.File) {
	fh, err := os.Open(os.ExpandEnv("$yd/index"))
	if err != nil {
		log.Fatalf("Couldn't open index")
	}
	defer fh.Close()
	
	bin := bufio.NewReader(fh)
	
	for {
		line, err := bin.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSuffix(line, "\n")
		if strings.HasSuffix(line, "+Win") {
			id := strings.TrimSpace(line[:11])
			eventfd, err := os.Open(os.ExpandEnv("$yd/" + id + "/event"))
			if err != nil {
				continue
			}
			ctlfd, err := os.OpenFile(os.ExpandEnv("$yd/" + id + "/ctl"), os.O_WRONLY, 0666)
			Must(err)
			return id, ctlfd, eventfd
		}
	}
	ctlfd, err := os.OpenFile(os.ExpandEnv("$yd/new/ctl"), os.O_RDWR, 0666)
	Must(err)
	ctlln := read(ctlfd)
	outbufid := strings.TrimSpace(ctlln[:11])
	eventfd, err := os.OpenFile(os.ExpandEnv("$yd/" + outbufid + "/event"), os.O_RDWR, 0666)
	Must(err)
	return outbufid, ctlfd, eventfd
}

func easyCommand(cmd string) bool {
	
	for _, c := range cmd {
		switch c {
		case '#', ';', '&', '|', '^', '$', '=', '\'', '`', '{', '}', '(', ')', '<', '>', '[', ']', '*', '?', '~':
			return false
		}
	}
	return true
}

func main() {
	outbufid, ctlfd, eventfd := findWin()
	bodyfd, err := os.OpenFile(os.ExpandEnv("$yd/" + outbufid + "/body"), os.O_WRONLY, 0666)
	Must(err)
	addrfd, err := os.OpenFile(os.ExpandEnv("$yd/" + outbufid + "/addr"), os.O_WRONLY, 0666)
	Must(err)
	xdatafd, err := os.OpenFile(os.ExpandEnv("$yd/" + outbufid + "/xdata"), os.O_WRONLY, 0666)
	Must(err)

	_, err = ctlfd.Write([]byte("name +Win\n"))
	Must(err)
	
	_, err = ctlfd.Write([]byte("dump " + strings.Join(os.Args, " ") + "\n"))
	Must(err)
	wd, _ := os.Getwd()
	_, err = ctlfd.Write([]byte("dumpdir " + wd + "\n"))
	
	
	var cmd *exec.Cmd
	if len(os.Args) > 1 {
		cmdstr := strings.Join(os.Args[1:], " ")
		if easyCommand(cmdstr) {
			vcmdstr := strings.Split(cmdstr, " ")
			cmd = exec.Command(vcmdstr[0], vcmdstr[1:]...)
		} else {
			cmd = exec.Command("/bin/sh", "-c",  cmdstr)
		}
	} else {
		shell := os.Getenv("yaccoshell")
		if shell == "" {
			shell = os.Getenv("SHELL")
		}
		if shell == "" {
			shell = "/bin/bash"
		}

		cmd = exec.Command(shell)
	}

	pty := run(cmd)

	go eventReader(eventfd, ctlfd, addrfd, bodyfd, pty, outbufid)
	go outputReader(cmd, pty, bodyfd, outbufid, ctlfd, addrfd, xdatafd)

	if debug {
		fmt.Println("Waiting for command to finish")
	}

	notifyChan := make(chan os.Signal)
	endChan := make(chan bool)
	signal.Notify(notifyChan, os.Interrupt, os.Kill)
	go notifyProc(notifyChan, endChan, bodyfd, ctlfd)

	cmd.Wait()
	endChan <- true
	if debug {
		log.Printf("Finished")
	}
	time.Sleep(1 * time.Second)
	os.Exit(0)
}
