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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
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
	fmt.Printf("awesome-sync: wrote %s: %d sections, %d entries, %d realms, from %s\n",
		*out, len(snap.Sections), snap.Count(), len(snap.Paths()), sha[:12])
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
