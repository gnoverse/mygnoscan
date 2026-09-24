// Command mcp-eval measures whether /mcp's tool descriptions lead a model to
// the right tool.
//
// The conformance suite checks the wire format, and a Go test can check that a
// tool returns what it should once it is called. Neither touches the part a
// model actually consumes: seven descriptions, and the choice it makes from
// them. That choice is the whole interface. A tool nobody picks is a tool that
// does not exist, and one picked for the wrong question is worse, because the
// answer looks authoritative.
//
// Not a CI job. It needs a model and therefore credentials, it costs real
// tokens, and its result is a percentage that moves: a run that occasionally
// scores 14 of 16 instead of 15 is not a broken build. Run it when the tool
// list or a description changes, and read the misses rather than the number.
//
//	go run ./cmd/mcp-eval -url http://127.0.0.1:8888/mcp
//
// The tool list is fetched from a running endpoint rather than read out of the
// source, so what is scored is exactly what a client would be handed.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

type question struct {
	Ask    string   `json:"ask"`
	Accept []string `json:"accept"`
	Prefer string   `json:"prefer"`
	Why    string   `json:"why,omitempty"`
}

type fixture struct {
	Questions []question `json:"questions"`
}

type tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	} `json:"inputSchema"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		url         = flag.String("url", "http://127.0.0.1:8888/mcp", "MCP endpoint to read the tool list from")
		fixturePath = flag.String("fixture", "mcp/tool-selection.json", "questions and their expected tool")
		modelCmd    = flag.String("model", "claude", "command that answers a prompt on stdin, one shot")
		modelArgs   = flag.String("model-args", "-p", "comma-separated arguments for that command")
		verbose     = flag.Bool("verbose", false, "print every answer, not only the misses")
	)
	flag.Parse()

	tools, err := fetchTools(*url)
	if err != nil {
		return fmt.Errorf("read the tool list from %s: %w", *url, err)
	}
	if len(tools) == 0 {
		return fmt.Errorf("%s served an empty tool list", *url)
	}

	raw, err := os.ReadFile(*fixturePath)
	if err != nil {
		return err
	}
	var fx fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		return fmt.Errorf("parse %s: %w", *fixturePath, err)
	}

	// Every question is asked against the same catalogue, and the catalogue is
	// built once: a model that saw a different list per question would be
	// scoring noise.
	catalogue := renderCatalogue(tools)
	names := map[string]bool{"none": true}
	for _, t := range tools {
		names[t.Name] = true
	}

	fmt.Printf("%d tools from %s, %d questions\n\n", len(tools), *url, len(fx.Questions))

	var accepted, preferred int
	var misses []string
	for i, q := range fx.Questions {
		answer, err := ask(*modelCmd, strings.Split(*modelArgs, ","), catalogue, q.Ask)
		if err != nil {
			return fmt.Errorf("question %d: %w", i+1, err)
		}
		pick := normalise(answer, names)

		ok := contains(q.Accept, pick)
		best := pick == q.Prefer
		switch {
		case best:
			accepted++
			preferred++
		case ok:
			accepted++
		}

		mark := "MISS"
		switch {
		case best:
			mark = "ok  "
		case ok:
			mark = "ok~ " // acceptable, not the one the descriptions steer toward
		}
		if !ok || *verbose {
			fmt.Printf("%s %-16s want %-16s %s\n", mark, pick, q.Prefer, q.Ask)
			if !ok && q.Why != "" {
				fmt.Printf("     %s\n", q.Why)
			}
		}
		if !ok {
			misses = append(misses, fmt.Sprintf("%s -> %s (want %s)", q.Ask, pick, q.Prefer))
		}
	}

	n := len(fx.Questions)
	fmt.Printf("\nacceptable %d/%d, preferred %d/%d\n", accepted, n, preferred, n)
	if len(misses) > 0 {
		fmt.Printf("\nRead these rather than the score:\n")
		for _, m := range misses {
			fmt.Printf("  - %s\n", m)
		}
	}
	// Deliberately exits 0 on a miss. This is a reading instrument, not a
	// gate: wiring a flaky percentage into a build teaches people to rerun it.
	return nil
}

func fetchTools(url string) ([]tool, error) {
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("endpoint answered %s", resp.Status)
	}
	var out struct {
		Result struct {
			Tools []tool `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("tools/list: %s", out.Error.Message)
	}
	return out.Result.Tools, nil
}

// renderCatalogue writes the tool list the way a client would present it: name,
// description and argument names. Not the raw JSON schema, because the point is
// to score the prose, and burying it in braces measures something else.
func renderCatalogue(tools []tool) string {
	var b strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Description)
		var args []string
		for k := range t.InputSchema.Properties {
			if contains(t.InputSchema.Required, k) {
				k += " (required)"
			}
			args = append(args, k)
		}
		sort.Strings(args)
		if len(args) > 0 {
			fmt.Fprintf(&b, "  arguments: %s\n", strings.Join(args, ", "))
		}
	}
	return b.String()
}

const promptTemplate = `You are choosing one tool to answer a question. Here are the tools available:

%s
Question: %s

Reply with the tool name and nothing else. If no tool here can answer it, reply: none`

func ask(cmd string, args []string, catalogue, q string) (string, error) {
	prompt := fmt.Sprintf(promptTemplate, catalogue, q)
	c := exec.Command(cmd, args...)
	c.Stdin = strings.NewReader(prompt)
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", cmd, err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

var wordRE = regexp.MustCompile(`[a-z_]+`)

// normalise pulls a tool name out of whatever the model said. A model that
// answers in a sentence has still chosen, and scoring that as a miss would
// measure its obedience rather than the descriptions.
func normalise(answer string, known map[string]bool) string {
	for _, w := range wordRE.FindAllString(strings.ToLower(answer), -1) {
		if known[w] {
			return w
		}
	}
	return strings.TrimSpace(strings.ToLower(answer))
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
