// Copyright 2016 Tom Thorogood. All rights reserved.
// Use of this source code is governed by a
// Modified BSD License license that can be found in
// the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"html"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	var timeout time.Duration
	flag.DurationVar(&timeout, "timeout", 2*time.Minute, "how long to keep open the commit listeners before timing out")

	var safe bool
	flag.BoolVar(&safe, "safe", true, "run jekyll with the --safe flag")

	var port int
	flag.IntVar(&port, "port", 8080, "the port to listen on")

	flag.Parse()

	repo := flag.Arg(0)

	if flag.NArg() != 1 || len(repo) == 0 {
		fmt.Printf("usage: %s <git repo>\n", os.Args[0])
		os.Exit(1)
	}

	var repoLock sync.Mutex

	repoDir, err := ioutil.TempDir("", "git.")
	if err != nil {
		panic(err)
	}

	defer os.RemoveAll(repoDir)

	outDir, err := ioutil.TempDir("", "site.")
	if err != nil {
		panic(err)
	}

	defer os.RemoveAll(outDir)

	cmd := exec.Command("git", "clone", repo, repoDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		panic(err)
	}

	cmd = exec.Command("git", "log", "--oneline")
	cmd.Dir = repoDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		panic(err)
	}

	var order []string
	commits := make(map[string]string)

	scanner := bufio.NewScanner(&out)

	for scanner.Scan() {
		line := strings.SplitN(scanner.Text(), " ", 2)
		commit := line[0]
		title := line[1]

		order = append(order, commit)
		commits[commit] = title
	}

	if err := scanner.Err(); err != nil {
		panic(err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<!DOCTYPE html>\n<title>%s</title>\n<p>%d commits:</p>\n<ul>\n", html.EscapeString(repo), len(commits))

		for _, commit := range order {
			title := commits[commit]
			fmt.Fprintf(w, "<li><a href=\"/commit/%[1]s\"><code>%[1]s</code> %[2]s</a></li>\n", commit, html.EscapeString(title))
		}

		fmt.Fprint(w, "</ul>")
	})

	done := make(map[string]struct{}, len(commits))
	var doneLock sync.RWMutex

	ports := make(map[string]string, len(commits))
	var portsLock sync.RWMutex

	http.HandleFunc("/commit/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(r.URL.Path[len("/commit/"):], "/", 2)
		commit := parts[0]
		var redirect string

		if len(parts) == 2 {
			redirect = parts[1]
		}

		if _, ok := commits[commit]; !ok {
			http.NotFound(w, r)
			return
		}

		defer func() {
			if err := recover(); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

				fmt.Printf("error: %v\n", err)
			}
		}()

		dir := path.Join(outDir, commit)

		doneLock.RLock()
		if _, ok := done[commit]; !ok {
			doneLock.RUnlock()

			repoLock.Lock()
			doneLock.Lock()
			if _, ok := done[commit]; !ok {
				cmd := exec.Command("git", "checkout", commit)
				cmd.Dir = repoDir
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr

				if err := cmd.Run(); err != nil {
					doneLock.Unlock()
					repoLock.Unlock()

					panic(err)
				}

				var safeFlag string
				if safe {
					safeFlag = "--safe"
				}

				cmd = exec.Command("jekyll", "build", safeFlag, "-s", repoDir, "-d", dir)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr

				if err := cmd.Run(); err != nil {
					doneLock.Unlock()
					repoLock.Unlock()

					panic(err)
				}

				done[commit] = struct{}{}
			}
			doneLock.Unlock()
			repoLock.Unlock()
		} else {
			doneLock.RUnlock()
		}

		portsLock.RLock()
		if port, ok := ports[commit]; ok {
			http.Redirect(w, r, "http://localhost:"+port+"/"+redirect, http.StatusSeeOther)

			portsLock.RUnlock()
			return
		} else {
			portsLock.RUnlock()
		}

		portsLock.Lock()
		if port, ok := ports[commit]; ok {
			http.Redirect(w, r, "http://localhost:"+port+"/"+redirect, http.StatusSeeOther)

			portsLock.Unlock()
			return
		}

		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			portsLock.Unlock()

			panic(err)
		}

		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		ports[commit] = port

		go func() {
			timer := time.AfterFunc(timeout, func() {
				fmt.Printf("%s listener timed out at :%s after %s of inactivity\n", commit, port, timeout)

				portsLock.Lock()
				delete(ports, commit)
				portsLock.Unlock()

				if err := ln.Close(); err != nil {
					panic(err)
				}
			})

			handler := http.FileServer(http.Dir(dir))
			server := &http.Server{
				Addr: ":" + port,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					timer.Reset(timeout)
					handler.ServeHTTP(w, r)
				}),
			}

			fmt.Printf("Listening for %s on :%s\n", commit, port)

			err := server.Serve(tcpKeepAliveListener{ln.(*net.TCPListener)})
			if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
				panic(err)
			}
		}()
		portsLock.Unlock()

		http.Redirect(w, r, "http://localhost:"+port+"/"+redirect, http.StatusSeeOther)
	})

	fmt.Printf("Listening on :%d\n", port)
	if err := http.ListenAndServe(":"+strconv.Itoa(port), nil); err != nil {
		panic(err)
	}
}
