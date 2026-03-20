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
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

type Nav struct {
	seg      string
	Name     string
	Path     string
	Items    []Nav
	selected bool
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

var (
	srcDir        string
	dstDir        string
	tplFile       string
	siteName      string
	ignorePat     string
	numWorkers    int
	shouldRebuild bool

	tpl      *template.Template
	ignoreRe *regexp.Regexp
	md       goldmark.Markdown
	dirCache map[string][]os.DirEntry
	wg       sync.WaitGroup
	navTree  []Nav
)

func main() {
	flag.StringVar(&srcDir, "i", "", "input dir (required)")
	flag.StringVar(&dstDir, "o", "", "output dir (required)")
	flag.StringVar(&tplFile, "t", "", "template file (required)")
	flag.StringVar(&siteName, "s", "", "site name (required)")
	flag.StringVar(&ignorePat, "x", "", "ignore files that match regexp")
	flag.IntVar(&numWorkers, "w", 1, "number of workers (0 for nproc)")
	flag.BoolVar(&shouldRebuild, "r", false, "rebuild all inputs")
	flag.Parse()

	missing := []string{}
	if srcDir == "" {
		missing = append(missing, "-src")
	}
	if dstDir == "" {
		missing = append(missing, "-dst")
	}
	if tplFile == "" {
		missing = append(missing, "-tpl")
	}
	if siteName == "" {
		missing = append(missing, "-site")
	}
	if len(missing) > 0 {
		fatal("missing %v, see -h for usage", missing)
	}

	err := os.MkdirAll(dstDir, 0755)
	check(err)

	tpl, err = template.ParseFiles(tplFile)
	check(err)

	if numWorkers == 0 {
		numWorkers = runtime.NumCPU()
	}

	if ignorePat != "" {
		var err error
		ignoreRe, err = regexp.Compile(ignorePat)
		check(err)
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

	dirCache = make(map[string][]os.DirEntry)

	navTree = mkNav(srcDir)

	jobs := make(chan Job)
	for i := 0; i < numWorkers; i++ {
		go worker(jobs)
	}
	build(srcDir, []string{}, jobs)
	close(jobs)
	wg.Wait()
}

func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func check(err error) {
	if err != nil {
		fatal("%s", err.Error())
	}
}

func mkNav(cwd string) []Nav {
	var n []Nav

	entries, err := os.ReadDir(cwd)
	check(err)

	dirCache[cwd] = entries

	for _, e := range entries {
		name := e.Name()
		if ignoreMatches(name) {
			continue
		}

		path := filepath.Join(cwd, name)
		rel, _ := filepath.Rel(srcDir, path)

		if e.IsDir() {
			n = append(n, Nav{
				seg:      name,
				Name:     name + "/",
				Path:     "/" + rel + "/",
				Items:    mkNav(path),
				selected: false,
			})
		} else {
			if isIndexFile(name) {
				continue
			}

			if hasMdExt(name) {
				n = append(n, Nav{
					seg:  name,
					Name: getPageName(name),
					Path: "/" + replaceMdWithHtmlExt(rel),
				})
			}
		}
	}

	return n
}

func ignoreMatches(s string) bool {
	return ignoreRe != nil && ignoreRe.MatchString(s)
}

func isIndexFile(s string) bool {
	return s == "index.md" || s == "index.html"
}

func hasMdExt(s string) bool {
	return strings.HasSuffix(s, ".md")
}

func getPageName(s string) string {
	return strings.ReplaceAll(strings.TrimSuffix(s, ".md"), "-", " ")
}

func replaceMdWithHtmlExt(s string) string {
	return strings.TrimSuffix(s, ".md") + ".html"
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
			`<sup class="fn-ref"><a class="fn-btn" href="#fn-` + label + `">` + label + `</a></sup>` +
			`<span class="sidenote"><span class="sn-num">` + label + `</span> ` + note + `</span>` +
			`<span class="fn-popup" id="fn-` + label + `"><span class="sn-num">` + label + `</span> ` + note + `</span>` +
		`</span>`
	})
	cleanBlock := reBackref.ReplaceAllString(block, "")
	out = strings.Replace(out, block, cleanBlock, 1)
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

		isMdFile := hasMdExt(j.srcPath)
		if isMdFile {
			j.dstPath = replaceMdWithHtmlExt(j.dstPath)
		}

		if !shouldRebuild {
			srcStat, err := srcFile.Stat()
			check(err)

			dstStat, err := os.Stat(j.dstPath)
			if err != nil {
				if !os.IsNotExist(err) {
					fmt.Println(err)
					os.Exit(1)
				}
			} else if !srcStat.ModTime().After(dstStat.ModTime()) {
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
	bwriter *bufio.Writer,
) {
	if !isMd {
		_, err := io.Copy(dst, src)
		check(err)

		return
	}

	path := src.Name()
	name := filepath.Base(path)

	var page Page
	page.SiteName = siteName

	if isIndexFile(name) {
		dir := filepath.Dir(path)
		if dir == srcDir {
			page.Name = siteName
		} else {
			page.Name = filepath.Base(dir)
		}
	} else {
		page.Name = getPageName(name)
	}

	bin.ReadFrom(src)

	err := md.Convert(bin.Bytes(), bout)
	check(err)

	htmlOut := bout.String()
	htmlOut = addSidenotes(htmlOut)
	page.Content = template.HTML(htmlOut)
	page.Items = getItems(navTree, walk)

	bwriter.Reset(dst)

	err = tpl.Execute(bwriter, page)
	check(err)

	bwriter.Flush()
}

func getItems(nav []Nav, walk []string) []Nav {
	if len(walk) == 0 {
		return nav
	}

	for i, n := range nav {
		if n.seg == walk[0] {
			item := n
			item.selected = true
			item.Items = getItems(n.Items, walk[1:])

			result := make([]Nav, len(nav))
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

		if ignoreMatches(name) {
			continue
		}

		src := filepath.Join(cwd, name)
		rel, _ := filepath.Rel(srcDir, src)
		dst := filepath.Join(dstDir, rel)

		if e.IsDir() {
			err := os.MkdirAll(dst, 0755)
			check(err)
			build(src, append(walk, name), jobs)
		} else {
			newWalk := append(walk, name)
			if isIndexFile(name) {
				newWalk = walk
			}

			wg.Add(1)
			jobs <- Job{
				srcPath: src,
				dstPath: dst,
				walk:    newWalk,
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
