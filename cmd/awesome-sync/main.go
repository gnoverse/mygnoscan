// Command awesome-sync regenerates pkg/registry/data/awesome.json from the
// gnoverse/awesome-gno README.
//
// Run it with `make awesome`. It is a generator rather than a runtime fetch on
// purpose: see the note at the top of pkg/registry/awesome.go. The output is
// committed, so a regeneration shows up as a reviewable diff of what the
// community changed, which is also the cheapest way to notice that a project
// was added or retired.
//
// It takes the network and nothing else, so it is never run by CI or by the
// server; a build that cannot reach GitHub still builds.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/moul/mygnoscan/pkg/registry"
)

const (
	owner = "gnoverse"
	repo  = "awesome-gno"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "awesome-sync:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		branch = flag.String("branch", "main", "branch to snapshot")
		out    = flag.String("out", "pkg/registry/data/awesome.json", "file to write")
		check  = flag.Bool("check", false, "compare against the committed file and fail if it moved, without writing")
	)
	flag.Parse()

	// The commit is resolved first and the README is then read *at that
	// commit*, not at the branch tip. Reading the tip twice would let a push
	// land between the two calls and stamp the file with a sha it does not
	// match, which is exactly the kind of quiet wrongness the sha exists to
	// rule out.
	sha, err := headCommit(*branch)
	if err != nil {
		return err
	}
	md, err := get(fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/README.md", owner, repo, sha))
	if err != nil {
		return err
	}
	sections, err := registry.ParseAwesome(string(md))
	if err != nil {
		return err
	}
	// The list links repositories; the page wants apps. This is the step that
	// turns one into the other, and it is the slow half of the run because it
	// talks to every project's own server.
	resolveSites(context.Background(), sections)

	snap := registry.Awesome{
		Source:   fmt.Sprintf("https://github.com/%s/%s", owner, repo),
		Readme:   fmt.Sprintf("https://github.com/%s/%s/blob/%s/README.md", owner, repo, *branch),
		Contrib:  fmt.Sprintf("https://github.com/%s/%s/blob/%s/CONTRIBUTING.md", owner, repo, *branch),
		Commit:   sha,
		Synced:   time.Now().UTC().Format("2006-01-02"),
		Sections: sections,
	}
	body, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	if *check {
		old, err := os.ReadFile(*out)
		if err != nil {
			return err
		}
		// The synced date changes on every run and says nothing about the
		// list, so it is not what "moved" means here.
		if sameList(old, body) {
			fmt.Printf("awesome-sync: up to date (%d entries, %s)\n", snap.Count(), sha[:12])
			return nil
		}
		return fmt.Errorf("awesome.json is behind %s@%s; run `make awesome`", *branch, sha[:12])
	}

	if err := os.WriteFile(*out, body, 0o644); err != nil {
		return err
	}
	apps := snap.Apps()
	fmt.Printf("awesome-sync: wrote %s: %d sections, %d entries, %d apps (%d realms, %d sites), from %s\n",
		*out, len(snap.Sections), snap.Count(), len(apps), len(snap.Paths()), len(snap.SiteHosts()), sha[:12])

	// Printed rather than left to be worked out, because it is configuration on
	// another box: gnoshot refuses a host it was not told about, and the failure
	// is a card with no picture and nothing in any log here.
	//
	// Both sources, not just the community list. apps.json names a website too,
	// and this line used to print the snapshot's hosts with a comment asking the
	// reader to check the other file by hand. Nobody did: kourt.xyz has been
	// curated since 2026-09-21 and its card has been photographing nothing ever
	// since, answering 400 with `host "kourt.xyz" not on the allowlist`, and
	// bubblerumble.net joined it the day it was added. A manual step that fails
	// silently on somebody else's box is a step that does not exist.
	//
	// The snapshot's half comes from `snap`, which is what was just written,
	// rather than from the embedded copy this binary was compiled against:
	// those differ by exactly the change being made, and printing the old one
	// after a sync is how this line would go stale in the one case it is for.
	hosts := map[string]bool{}
	for _, h := range snap.SiteHosts() {
		hosts[h] = true
	}
	reg, err := registry.Load()
	if err != nil {
		return fmt.Errorf("reading apps.json back for the host list: %w", err)
	}
	for _, a := range reg.Apps {
		u, err := url.Parse(a.URL)
		if err != nil || u.Hostname() == "" {
			continue
		}
		hosts[strings.ToLower(u.Hostname())] = true
	}
	all := make([]string, 0, len(hosts))
	for h := range hosts {
		all = append(all, h)
	}
	sort.Strings(all)
	fmt.Printf("\ngnoshot needs these hosts on its -allow-site flag:\n\n  -allow-site '%s'\n",
		strings.Join(all, ","))
	return nil
}

// sameList compares two snapshots ignoring the fields that move on their own.
func sameList(a, b []byte) bool {
	strip := func(raw []byte) string {
		var doc registry.Awesome
		if err := json.Unmarshal(raw, &doc); err != nil {
			return string(raw)
		}
		doc.Synced = ""
		out, _ := json.Marshal(doc)
		return string(out)
	}
	return strip(a) == strip(b)
}

func headCommit(branch string) (string, error) {
	body, err := get(fmt.Sprintf("https://api.github.com/repos/%s/%s/commits/%s", owner, repo, branch))
	if err != nil {
		return "", err
	}
	var doc struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", err
	}
	if len(doc.SHA) != 40 {
		return "", fmt.Errorf("github returned sha %q for %s", doc.SHA, branch)
	}
	return doc.SHA, nil
}

func get(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// Unauthenticated GitHub is 60 requests an hour per IP, which is plenty for
	// a generator run by hand, but a token makes it 5,000 and costs one line.
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}
