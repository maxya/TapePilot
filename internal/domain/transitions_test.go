package domain

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestContractMatchesArchitecture(t *testing.T) {
	data, err := os.ReadFile("../../docs/ARCHITECTURE.md")
	if err != nil {
		t.Fatal(err)
	}
	_, diagram, ok := strings.Cut(string(data), "stateDiagram-v2")
	if !ok {
		t.Fatal("missing state diagram")
	}
	diagram, _, _ = strings.Cut(diagram, "```")
	edges := map[[2]State]bool{}
	for _, match := range regexp.MustCompile(`(?m)^  ([A-Z_]+) --> ([A-Z_]+)`).FindAllStringSubmatch(diagram, -1) {
		edges[[2]State{State(match[1]), State(match[2])}] = true
	}
	for _, e := range TransitionContract() {
		if e.From == Preflight && e.To == Cancelled {
			if e.Requirement == "" {
				t.Fatal("missing cancellation evidence requirement")
			}
			continue
		}
		key := [2]State{e.From, e.To}
		if !edges[key] || e.Requirement == "" {
			t.Fatalf("unexpected edge %#v", e)
		}
		delete(edges, key)
	}
	if len(edges) != 0 {
		t.Fatalf("missing edges: %v", edges)
	}
}
