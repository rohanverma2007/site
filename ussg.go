// SPDX-License-Identifier: BSD-2-Clause
//
// Copyright (c) 2026, Faisal Al Ammar
// All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are met:
//
// 1. Redistributions of source code must retain the above copyright notice,
//    this list of conditions and the following disclaimer.
// 2. Redistributions in binary form must reproduce the above copyright
//    notice, this list of conditions and the following disclaimer in the
//    documentation and/or other materials provided with the distribution.
//
// THIS SOFTWARE IS PROVIDED BY THE AUTHOR AND CONTRIBUTORS ``AS IS'' AND ANY
// EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
// WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
// DISCLAIMED. IN NO EVENT SHALL THE AUTHOR OR CONTRIBUTORS BE LIABLE FOR ANY
// DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
// (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
// SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
// CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT
// LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY
// OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH
// DAMAGE.

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/extension"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"regexp"
)

var (
	args     Args
	nav      []Nav
	dirCache = map[string][]os.DirEntry{}
	wg       sync.WaitGroup
	md       goldmark.Markdown
	tpl      *template.Template
)

type Args struct {
	srcDir        string
	dstDir        string
	tplFile       string
	siteName      string
	nWorkers      int
	shouldRebuild bool
}

type Nav struct {
	Seg      string
	Name     string
	Path     string
	Items    []Nav
	Selected bool
}

type Page struct {
	SiteName string
	Name     string
	Content  template.HTML
	Items    []Nav
}

type Job struct {
	srcPath string
	dstPath string
	walk    []string
}

func main() {
	flag.StringVar(&args.srcDir, "src", "", "source directory")
	flag.StringVar(&args.dstDir, "dst", "", "destination directory")
	flag.StringVar(&args.tplFile, "tpl", "", "template file")
	flag.StringVar(&args.siteName, "site", "", "site name")
	flag.IntVar(&args.nWorkers, "nworkers", 1, "number of workers to spawn")
	flag.BoolVar(&args.shouldRebuild, "reb", false, "rebuild all sources")

	flag.Parse()

	if args.srcDir == "" {
		fatal("missing -src")
	}
	if args.dstDir == "" {
		fatal("missing -dst")
	}
	if args.tplFile == "" {
		fatal("missing -tpl")
	}
	if args.siteName == "" {
		fatal("missing -site")
	}

	err := os.MkdirAll(args.dstDir, 0755)
	check(err)

	tpl, err = template.ParseFiles(args.tplFile)
	check(err)

	if args.nWorkers == 0 {
		args.nWorkers = runtime.NumCPU()
	}

	md = goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			extension.Footnote,
		),
		goldmark.WithRendererOptions(
			html.WithUnsafe(),
		),
	)

	nav = mkNav(args.srcDir)

	jobs := make(chan Job)

	for i := 0; i < args.nWorkers; i++ {
		go worker(jobs)
	}

	build(args.srcDir, []string{}, jobs)

	close(jobs)

	wg.Wait()
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func check(e error) {
	if e != nil {
		fatal(e.Error())
	}
}

func mkNav(cwd string) []Nav {
	var n []Nav

	entries, err := os.ReadDir(cwd)
	check(err)

	dirCache[cwd] = entries

	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(cwd, name)
		relPath, _ := filepath.Rel(args.srcDir, path)

		if e.IsDir() {
			n = append(n, Nav{
				Seg:      name,
				Name:     name + "/",
				Path:     "/" + relPath + "/",
				Items:    mkNav(path),
				Selected: false,
			})
		} else {
			if name == "index.md" || name == "index.html" {
				continue
			}

			if strings.HasSuffix(name, ".md") {
				n = append(n, Nav{
					Seg:  name,
					Name: getPageName(name),
					Path: "/" + strings.TrimSuffix(
						relPath,
						".md",
					) + ".html",
				})
			}
		}
	}

	return n
}

func getPageName(s string) string {
	return strings.ReplaceAll(strings.TrimSuffix(s, ".md"), "-", " ")
}

func addSidenotes(in string) string {
	reFootnoteBlock := regexp.MustCompile(`(?s)<div class="footnotes"[^>]*>.*?</div>`)
	reFootnoteItem := regexp.MustCompile(`(?s)<li id="fn:?([A-Za-z0-9_-]+)">(.*?)</li>`)
	reBackref := regexp.MustCompile(`(?s)\s*<a[^>]*href="#fnref:?[^"]+"[^>]*>.*?</a>\s*`)
	reRef := regexp.MustCompile(`(?s)<sup id="fnref:?([A-Za-z0-9_-]+)"><a[^>]*href="#fn:?([A-Za-z0-9_-]+)"[^>]*>(.*?)</a></sup>`)
	reOuterP := regexp.MustCompile(`(?s)^<p>(.*?)</p>$`)

	block := reFootnoteBlock.FindString(in)
	if block == "" {
		return in
	}

	notes := make(map[string]string)

	matches := reFootnoteItem.FindAllStringSubmatch(block, -1)
	for _, m := range matches {
		key := m[1]
		body := strings.TrimSpace(m[2])
		body = reBackref.ReplaceAllString(body, "")
		body = strings.TrimSpace(body)
		body = reOuterP.ReplaceAllString(body, `$1`)
		notes[key] = body
	}

	out := reRef.ReplaceAllStringFunc(in, func(s string) string {
		m := reRef.FindStringSubmatch(s)
		if len(m) < 4 {
			return s
		}

		refID := m[1]
		noteID := m[2]
		label := strings.TrimSpace(m[3])

		note, ok := notes[noteID]
		if !ok {
			note, ok = notes[refID]
			if !ok {
				return s
			}
		}

		if label == "" {
			label = noteID
		}

		return `<span class="fn-wrap">` +
			`<sup class="fn-ref"><button class="fn-btn" type="button" aria-expanded="false">` + label + `</button></sup>` +
			`<span class="sidenote"><span class="sn-num">` + label + `</span> ` + note + `</span>` +
			`<span class="fn-popup"><span class="sn-num">` + label + `</span> ` + note + `</span>` +
			`</span>`
	})
	out = reFootnoteBlock.ReplaceAllString(out, "")
	return out
}

func worker(jobs <-chan Job) {
	var w bufio.Writer
	var bin bytes.Buffer
	var bout bytes.Buffer

	for j := range jobs {
		bin.Reset()
		bout.Reset()

		srcFile, err := os.Open(j.srcPath)
		check(err)

		isMdFile := strings.HasSuffix(j.srcPath, ".md")
		if isMdFile {
			j.dstPath = strings.TrimSuffix(
				j.dstPath, ".md",
			) + ".html"
		}

		if !args.shouldRebuild {
			srcStat, err := srcFile.Stat()
			check(err)

			dstStat, err := os.Stat(j.dstPath)
			if err != nil {
				if !os.IsNotExist(err) {
					fmt.Println(err)
					os.Exit(1)
				}
			} else if !srcStat.ModTime().After(
				dstStat.ModTime(),
			) {
				srcFile.Close()
				wg.Done()

				continue
			}
		}

		dstFile, err := os.OpenFile(
			j.dstPath,
			os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
			0644,
		)
		check(err)

		procFile(srcFile, dstFile, j.walk, isMdFile, &bin, &bout, &w)

		srcFile.Close()
		dstFile.Close()
		wg.Done()
	}
}

func procFile(
	src, dst *os.File,
	walk []string,
	isMd bool,
	bin, bout *bytes.Buffer,
	w *bufio.Writer,
) {
	if !isMd {
		_, err := io.Copy(dst, src)
		check(err)
		return
	}

	path := src.Name()
	name := filepath.Base(path)

	var p Page
	p.SiteName = args.siteName

	if name == "index.md" {
		dir := filepath.Dir(path)
		if dir == args.srcDir {
			p.Name = args.siteName
		} else {
			p.Name = filepath.Base(dir)
		}
	} else {
		p.Name = getPageName(name)
	}

	bin.ReadFrom(src)

	err := md.Convert(bin.Bytes(), bout)
	check(err)

	htmlOut := bout.String()
	htmlOut = addSidenotes(htmlOut)
	p.Content = template.HTML(htmlOut)
	p.Items = getItems(nav, walk)

	w.Reset(dst)

	err = tpl.Execute(w, p)
	check(err)

	w.Flush()
}

func getItems(nav []Nav, walk []string) []Nav {
	if len(walk) == 0 {
		return nav
	}

	for i, n := range nav {
		if n.Seg == walk[0] {
			item := n
			item.Selected = true
			item.Items = getItems(n.Items, walk[1:])

			result := make([]Nav, i+1)
			copy(result, nav[:i])
			result[i] = item

			return result
		}
	}

	return nav
}

func build(cwd string, walk []string, jobs chan<- Job) {
	entries := readDir(cwd)
	for _, e := range entries {
		name := e.Name()
		srcPath := filepath.Join(cwd, name)
		relPath, _ := filepath.Rel(args.srcDir, srcPath)
		dstPath := filepath.Join(args.dstDir, relPath)

		if e.IsDir() {
			err := os.MkdirAll(dstPath, 0755)
			check(err)

			build(srcPath, append(walk, name), jobs)
		} else {
			wg.Add(1)

			jobs <- Job{
				srcPath: srcPath,
				dstPath: dstPath,
				walk:    walk,
			}
		}
	}
}

func readDir(path string) []os.DirEntry {
	if v, ok := dirCache[path]; ok {
		return v
	}

	entries, err := os.ReadDir(path)
	check(err)

	dirCache[path] = entries

	return entries
}
