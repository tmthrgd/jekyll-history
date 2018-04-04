// Copyright 2018 Tom Thorogood. All rights reserved.
// Use of this source code is governed by a
// Modified BSD License license that can be found in
// the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/lunixbochs/vtclean"
	"github.com/tmthrgd/httphandlers"
)

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<meta charset=utf-8>
<title>{{.Title}}</title>
<style>body{margin:40px auto;max-width:650px;line-height:1.6;font-size:18px;color:#444;padding:0 10px}h1,h2,h3{line-height:1.2}</style>
<h1>{{.Title}}</h1>
<p>{{len .Commits}} commits:</p>
<ul>
{{- range .OrderedCommits}}
<li><a href="/commit/{{.}}/"><code>{{.}}</code> {{index $.Commits .}}</a></li>
{{- end}}
</ul>`))

var error404 = `<!doctype html>
<meta charset=utf-8>
<title>404 Not Found</title>
<style>body{margin:40px auto;max-width:650px;line-height:1.6;font-size:18px;color:#444;padding:0 10px}h1,h2,h3{line-height:1.2}</style>
<h1>404 Not Found</h1>
<p>The requested file was not found.</p>`

var buildErrorTmpl = template.Must(template.New("error-build").Parse(`<!doctype html>
<meta charset=utf-8>
<title>Error building {{.Commit}}</title>
<style>body{margin:40px auto;max-width:1200px;line-height:1.6;font-size:18px;color:#444;padding:0 10px}h1,h2,h3{line-height:1.2}pre{overflow:auto}</style>
<p>Error building {{.Commit}}:</p>
<pre>{{.Msg}}</pre>`))

var (
	favicon = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x10\x00\x00\x00\x10\b\x06\x00\x00\x00\x1f\xf3\xffa\x00\x00\x00\x16IDATx\xdacd\xa0\x100\x8e\x1a0j\xc0\xa8\x01\xc3\xc5\x00\x00&\x98\x00\x11\x9b\x92AZ\x00\x00\x00\x00IEND\xaeB`\x82"
	robots  = "User-agent: *\nDisallow: /"
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s: <git repo>\n", os.Args[0])
		flag.PrintDefaults()
	}
}

func main() {
	var safe bool
	flag.BoolVar(&safe, "safe", true, "run jekyll with the --safe flag")

	var port int
	flag.IntVar(&port, "port", 8080, "the port to listen on")

	flag.Parse()

	repo := flag.Arg(0)

	if flag.NArg() != 1 || repo == "" {
		flag.Usage()
		os.Exit(1)
	}

	for _, name := range [...]string{"git", "jekyll"} {
		if _, err := exec.LookPath(name); err != nil {
			log.Fatal(err)
		}
	}

	repoDir, err := ioutil.TempDir("", "repo.")
	if err != nil {
		log.Fatal(err)
	}

	defer os.RemoveAll(repoDir)

	outDir, err := ioutil.TempDir("", "site.")
	if err != nil {
		log.Fatal(err)
	}

	defer os.RemoveAll(outDir)

	cmd := exec.Command("git", "clone", repo, repoDir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr

	if err := cmd.Run(); err != nil {
		log.Fatal(err)
	}

	cmd = exec.Command("git", "log", "--oneline")
	cmd.Dir = repoDir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, os.Stderr

	if err := cmd.Run(); err != nil {
		log.Fatal(err)
	}

	var orderedCommits []string
	commits := make(map[string]string)

	scanner := bufio.NewScanner(&out)

	for scanner.Scan() {
		line := strings.SplitN(scanner.Text(), " ", 2)
		commit, title := line[0], line[1]

		orderedCommits = append(orderedCommits, commit)
		commits[commit] = title
	}

	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}

	notFoundHandler := handlers.ServeError(http.StatusNotFound, []byte(error404), "text/html; charset=utf-8")

	hosts := &handlers.SafeHostSwitch{
		NotFound: notFoundHandler,
	}

	router := chi.NewRouter()
	router.Use(middleware.GetHead)
	router.NotFound(notFoundHandler.ServeHTTP)

	hosts.Add("127.0.0.1", router)
	hosts.Add("::1", router)
	hosts.Add("localhost", router)

	now := time.Now()

	index, err := handlers.ServeTemplate("index.html", now, indexTmpl, struct {
		Title          string
		OrderedCommits []string
		Commits        map[string]string
	}{
		Title:          repo,
		OrderedCommits: orderedCommits,
		Commits:        commits,
	})
	if err != nil {
		log.Fatal(err)
	}

	router.Get("/", index.ServeHTTP)
	router.Get("/commit/{commit}/*", (&commitHandler{
		safe:     safe,
		port:     port,
		repoDir:  repoDir,
		outDir:   outDir,
		commits:  commits,
		hosts:    hosts,
		notFound: notFoundHandler,
	}).ServeHTTP)
	router.Get("/favicon.ico", handlers.ServeString("favicon.png", now, favicon).ServeHTTP)
	router.Get("/robots.txt", handlers.ServeString("robots.txt", now, robots).ServeHTTP)

	handler := handlers.AccessLog(hosts, nil)
	handler = &handlers.SecurityHeaders{
		Handler: handler,
	}
	handler = handlers.SetHeader(handler, "Server", "jekyll-history")

	fmt.Printf("Listening on :%d\n", port)

	srv := &http.Server{
		Addr:    ":" + strconv.Itoa(port),
		Handler: handler,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// termination handler
	term := make(chan os.Signal, 1)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)
	<-term

	// gracefull shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("error shutting down: %v", err)
	}
}

type commitHandler struct {
	safe     bool
	port     int
	repoDir  string
	outDir   string
	commits  map[string]string
	hosts    *handlers.SafeHostSwitch
	notFound http.Handler

	build    sync.Map
	repoLock sync.Mutex
}

func (ch *commitHandler) buildSite(commit, dir string) error {
	var stderr bytes.Buffer
	stderrClean := vtclean.NewWriter(&stderr, false)
	stderrWriter := io.MultiWriter(os.Stderr, stderrClean)

	ch.repoLock.Lock()
	defer ch.repoLock.Unlock()

	cmd := exec.Command("git", "checkout", commit)
	cmd.Dir = ch.repoDir
	cmd.Stdout, cmd.Stderr = os.Stdout, stderrWriter

	if err := cmd.Run(); err != nil {
		stderrClean.Close()
		return &stderrError{err, stderr.Bytes()}
	}

	stderr.Reset()

	var safeFlag string
	if ch.safe {
		safeFlag = "--safe"
	}

	cmd = exec.Command("jekyll", "build", safeFlag, "-s", ch.repoDir, "-d", dir)
	cmd.Dir = ch.repoDir
	cmd.Stdout, cmd.Stderr = os.Stdout, stderrWriter

	if err := cmd.Run(); err != nil {
		stderrClean.Close()
		return &stderrError{err, stderr.Bytes()}
	}

	return nil
}

func (ch *commitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	params := chi.RouteContext(r.Context())
	commit, redirect := params.URLParam("commit"), params.URLParam("*")

	if _, ok := ch.commits[commit]; !ok {
		ch.notFound.ServeHTTP(w, r)
		return
	}

	v, ok := ch.build.Load(commit)
	if !ok {
		v, _ = ch.build.LoadOrStore(commit, new(buildCommitOnce))
	}

	host, err := v.(*buildCommitOnce).Do(ch, commit)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)

		buildErrorTmpl.Execute(w, struct {
			Commit, Msg string
		}{
			Commit: commit,
			Msg:    err.Error(),
		})

		if se, ok := err.(*stderrError); ok {
			err = se.err
		}

		log.Printf("Error serving %s: %s", commit, err)
		return
	}

	url := &url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(host, strconv.Itoa(ch.port)),
		Path:     redirect,
		RawQuery: r.URL.RawQuery,
	}
	http.Redirect(w, r, url.String(), http.StatusSeeOther)
}

type buildCommitOnce struct {
	once sync.Once
	host string
	err  error
}

func (bc *buildCommitOnce) Do(ch *commitHandler, commit string) (string, error) {
	bc.once.Do(func() {
		dir := path.Join(ch.outDir, commit)

		if bc.err = ch.buildSite(commit, dir); bc.err != nil {
			return
		}

		handler := siteHandler(dir)

		var ip [net.IPv4len]byte
		one := [1]byte{1}

		h := fnv.New32a()
		io.WriteString(h, commit)

		for {
			h.Sum(ip[:0])
			ip[0] = 127

			bc.host = net.IP(ip[:]).String()

			if ch.hosts.Add(bc.host, handler) == nil {
				break
			}

			h.Write(one[:])
		}
	})

	return bc.host, bc.err
}

func siteHandler(dir string) http.Handler {
	notFound := handlers.ErrorCode(http.StatusNotFound)

	if f, err := http.Dir(dir).Open("/404.html"); err == nil {
		defer f.Close()

		if content, err := ioutil.ReadAll(f); err == nil {
			notFound = handlers.ServeError(http.StatusNotFound, content, "text/html; charset=utf-8")
		}
	}

	handler := http.FileServer(http.Dir(dir))
	return handlers.StatusCodeSwitch(handler, map[int]http.Handler{
		http.StatusNotFound: notFound,
	})
}

type stderrError struct {
	err    error
	stderr []byte
}

func (e *stderrError) Error() string {
	if len(e.stderr) == 0 {
		return e.err.Error()
	}

	stderr := bytes.Replace(e.stderr, nl, nltb, -1)
	stderr = bytes.TrimRight(stderr, "\t")
	return fmt.Sprintf("%s:\n\t%s", e.err, stderr)
}

var (
	nl   = []byte{'\n'}
	nltb = []byte{'\n', '\t'}
)
