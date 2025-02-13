// Copyright 2020 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package node

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrThrown is returned where the input script from Run() throws a Javascript
// error.
type ErrThrown struct {
	err error
}

func (err ErrThrown) Error() string {
	return err.err.Error()
}

// VM is a Javascript Virtual Machine running on Node.js
type VM interface {
	Run(javascript string) Value
}

// Value is the returned from Run().
type Value interface {
	// Error returns an error, if any.
	Error() error
	// String returns the string representation of the Value.
	String() string
}

// Options for VM
type Options struct {
	// OnEmit is an optional callback that handles emitted messages.
	OnEmit func(thing string)
	// OnConsoleLog is an optional callback that handles notice messages that
	// are intended for the console logger.
	OnLog func(msg string)
	// OnConsoleError is an optional callback that handles error messages that
	// are intended for the console logger.
	OnError func(msg string)
	// Dir is the working directory for the VM. Default is the same working
	// directory and currently running Go process.
	Dir string
	// Flags are the additional startup flags that are provided to the
	// "node" process.
	Flags []string
	// Inject is an optional string to inject Javascript code into the Node.js VM.
	Inject string
}

func spreadPointer[Type any](values ...Type) *Type {
	if len(values) == 0 {
		return nil
	}

	return &values[0]
}

// Returns a Javascript Virtual Machine running an isolated process of
// Node.js.
func NewNodeJS(options ...Options) (VM, error) {
	option := spreadPointer(options...)

	emit := func(thing string) {}
	var onError func(msg string)
	var onLog func(msg string)

	js := fmt.Sprintf(jsRuntime, "")
	if option != nil {
		js = fmt.Sprintf(jsRuntime, option.Inject)
	}

	flags := []string{"--experimental-detect-module", "--title", "go-node.mjs", "-e", js}
	if option != nil {
		emit = option.OnEmit
		flags = append(flags, option.Flags...)
		onError = option.OnError
		onLog = option.OnLog
	}
	cmd := exec.Command("node", flags...)
	if option != nil && option.Dir != "" {
		cmd.Dir = option.Dir
	}
	stderr, _ := cmd.StderrPipe()
	stdout, _ := cmd.StdoutPipe()
	stdin, _ := cmd.StdinPipe()
	var uidb [16]byte
	rand.Read(uidb[:])
	token := hex.EncodeToString(uidb[:])
	rand.Read(uidb[:])
	socket := os.TempDir() + "/" + hex.EncodeToString(uidb[:]) + ".sock"
	// Run stdout/stderr readers
	closech := make(chan bool, 2)
	var emsgmu sync.Mutex
	var emsgready bool
	var emsg string
	go func() {
		rd := bufio.NewReader(stderr)
		for {
			line, err := rd.ReadBytes('\n')
			if err != nil {
				break
			}
			emsgmu.Lock()
			if emsgready {
				if onError != nil {
					onError(string(line[:len(line)-1]))
				} else {
					os.Stderr.Write(line)
				}
			} else {
				emsg += string(line)
			}
			emsgmu.Unlock()
		}
		closech <- true
	}()
	go func() {
		rd := bufio.NewReader(stdout)
		for {
			line, err := rd.ReadBytes('\n')
			if err != nil {
				break
			}
			if strings.HasPrefix(string(line), token) {
				emit(string(line[len(token) : len(line)-1]))
			} else {
				if onLog != nil {
					onLog(string(line[:len(line)-1]))
				} else {
					os.Stdout.Write(line)
				}
			}
		}
	}()
	// Start the process
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Connect the Node instance. Perform a handshake.
	var conn net.Conn
	var rd *bufio.Reader
	if err := func() error {
		defer os.Remove(socket)
		ln, err := net.Listen("unix", socket)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdin, "%s:%s\n", token, socket)
		if err != nil {
			return err
		}
		var done int32
		go func() {
			select {
			case <-closech:
				atomic.StoreInt32(&done, 1)
				ln.Close()
			}
		}()
		conn, err = ln.Accept()
		if err != nil {
			if atomic.LoadInt32(&done) == 1 {
				emsgmu.Lock()
				defer emsgmu.Unlock()
				emsg = strings.TrimSpace(emsg)
				if emsg != "" {
					return errors.New(emsg)
				}
				return errors.New("runtime failed to initiate")
			}
			return err
		}
		rd = bufio.NewReader(conn)
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return err
		}
		parts := strings.Split(string(line), " ")
		if parts[0] != token {
			return errors.New("invalid handshake")
		}
		vers := strings.Split(parts[1][:len(parts[1])-1], ".")
		if !strings.HasPrefix(vers[0], "v") {
			return errors.New("invalid handshake")
		}
		emsgmu.Lock()
		emsgready = true
		emsgmu.Unlock()
		closech <- true
		return nil
	}(); err != nil {
		if conn != nil {
			conn.Close()
		}
		stdin.Close()
		return nil, err
	}
	// Start the runtime loop
	ch := make(chan *jsValue)
	go func() {
		defer func() {
			stdin.Close()
			conn.Close()
		}()
		for {
			v := <-ch
			if v == nil {
				return
			}
			vals := []*jsValue{v}
			var done bool
			for !done {
				select {
				case v := <-ch:
					if v == nil {
						return
					}
					vals = append(vals, v)
				default:
					done = true
				}
			}
			var buf []byte
			for _, val := range vals {
				data, _ := json.Marshal(val.js)
				buf = append(buf, data...)
				buf = append(buf, '\n')
				val.js = "" // release the script
			}
			if _, err := conn.Write(buf); err != nil {
				for _, val := range vals {
					val.err = err
					val.wg.Done()
					val.vm = nil // release the vm
				}
			} else {
				for _, val := range vals {
					var msg string
					data, err := rd.ReadBytes('\n')
					if err != nil {
						val.err = err
					} else if err := json.Unmarshal(data, &msg); err != nil {
						val.err = err
					} else if msg != "" && msg[0] == 'e' {
						val.err = ErrThrown{fmt.Errorf("%s", msg[1:])}
					} else if msg != "" && msg[0] == 'v' {
						val.ret = string(msg[1:])
					} else {
						val.err = errors.New("invalid response")
					}
					val.wg.Done()
					val.vm = nil // release the vm
				}
			}
		}
	}()

	vm := &nodeJsVM{
		ch: ch,
	}

	runtime.SetFinalizer(vm, func(_ *nodeJsVM) { ch <- nil })

	return vm, nil
}

type nodeJsVM struct {
	ch chan *jsValue
}

// Run some Javascript code. Returns the JSON or an error.
func (vm *nodeJsVM) Run(js string) Value {
	v := new(jsValue)
	v.vm = vm
	v.js = js
	v.wg.Add(1)
	vm.ch <- v
	return v
}

type jsValue struct {
	vm  *nodeJsVM
	js  string
	wg  sync.WaitGroup
	ret string
	err error
}

func (v *jsValue) Error() error {
	v.wg.Wait()
	return v.err
}

func (v *jsValue) String() string {
	v.wg.Wait()
	return v.ret
}

const jsRuntime = `
import { Socket } from "node:net";
import { createInterface } from "node:readline";
import { createRequire } from "node:module";

var require = createRequire(import.meta.url);

global.require = require;

%s

(function () {
  const rl = createInterface({ input: process.stdin });

  rl.on("line", function (line) {
    const socket = new Socket();
    const token = line.split(":")[0];

    socket.connect(line.split(":")[1], function () {
      socket.write(token + " " + process.version + "\n");

      global.emit = function (arg) {
        console.log(token + arg);
      };

      let input = Buffer.alloc(0);
      let output = Buffer.alloc(0);

      socket.on("data", async (data) => {
        input = Buffer.concat([input, data]);
        while (input.length > 0) {
          let idx = input.indexOf(10);
          if (idx == -1) {
            break;
          }

          const js = JSON.parse(input.slice(0, idx).toString("utf8"));
          input = input.slice(idx + 1);

          if (input.length == 0) {
            input = Buffer.alloc(0);
          }

          let ret;
          try {
            ret = "v" + (await import('data:text/javascript;charset=utf-8,' + encodeURIComponent(js))).default;
          } catch (e) {
            ret = "e" + e;
          }
          output = Buffer.concat([
            output,
            Buffer.from(JSON.stringify(ret) + "\n", "utf8"),
          ]);
        }
        if (output.length > 0) {
          socket.write(output);
          output = output.slice(0, 0);
        }
      });
    });
  });
})();
`
