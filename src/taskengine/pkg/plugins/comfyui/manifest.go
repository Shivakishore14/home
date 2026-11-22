package comfyui

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest describes a workflow template and the parameters clients may set.
// It lives next to the API-format workflow JSON, e.g.
// tasks/assets/comfyui/jeveration_t2i.yaml + jeveration_t2i.json.
type Manifest struct {
	Title       string           `yaml:"title" json:"title"`
	Description string           `yaml:"description" json:"description,omitempty"`
	Workflow    string           `yaml:"workflow" json:"workflow"`
	Params      map[string]Param `yaml:"params" json:"params"`
}

// Param maps a client-facing parameter onto a node input.
type Param struct {
	Node     string        `yaml:"node" json:"node"`
	Input    string        `yaml:"input" json:"input"`
	Type     string        `yaml:"type" json:"type"` // text, int, number, bool, seed, choice, image
	Label    string        `yaml:"label" json:"label,omitempty"`
	Required bool          `yaml:"required" json:"required,omitempty"`
	Default  interface{}   `yaml:"default" json:"default,omitempty"`
	Min      *float64      `yaml:"min" json:"min,omitempty"`
	Max      *float64      `yaml:"max" json:"max,omitempty"`
	MaxLen   int           `yaml:"max_length" json:"max_length,omitempty"`
	Options  []interface{} `yaml:"options" json:"options,omitempty"`
}

// Graph is an API-format ComfyUI workflow: node id -> node.
type Graph map[string]*Node

// Node is one ComfyUI node in API format.
type Node struct {
	ClassType string                 `json:"class_type"`
	Inputs    map[string]interface{} `json:"inputs"`
	Meta      json.RawMessage        `json:"_meta,omitempty"`
}

// FileInput is an upload-backed parameter to stage into ComfyUI's input dir.
type FileInput struct {
	Param    string
	Node     string
	Input    string
	URL      string
	Filename string
	MIME     string
}

var workflowName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ParseManifest parses and validates a manifest against its graph.
func ParseManifest(data []byte, graph Graph) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	for name, p := range m.Params {
		node, ok := graph[p.Node]
		if !ok {
			return nil, fmt.Errorf("param %q: node %q not in workflow", name, p.Node)
		}
		if p.Input == "" {
			return nil, fmt.Errorf("param %q: input is required", name)
		}
		if _, ok := node.Inputs[p.Input]; !ok {
			return nil, fmt.Errorf("param %q: node %q (%s) has no input %q", name, p.Node, node.ClassType, p.Input)
		}
		switch p.Type {
		case "text", "int", "number", "bool", "seed", "choice", "image":
		default:
			return nil, fmt.Errorf("param %q: unknown type %q", name, p.Type)
		}
	}
	return &m, nil
}

// ParseGraph parses an API-format workflow and rejects UI-format exports.
func ParseGraph(data []byte) (Graph, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	if _, ui := probe["nodes"]; ui {
		return nil, fmt.Errorf("workflow is in UI format; export it with \"Export (API)\"")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var g Graph
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	for id, n := range g {
		if n == nil || n.ClassType == "" {
			return nil, fmt.Errorf("workflow node %q has no class_type", id)
		}
	}
	return g, nil
}

// Apply validates client inputs against the manifest and writes them into
// the graph. Unknown inputs are rejected. It returns the resolved values
// (for reporting) and file inputs to stage.
func (m *Manifest) Apply(g Graph, inputs map[string]json.RawMessage) (map[string]interface{}, []FileInput, error) {
	for name := range inputs {
		if _, ok := m.Params[name]; !ok {
			return nil, nil, fmt.Errorf("unknown parameter %q (allowed: %s)", name, strings.Join(m.paramNames(), ", "))
		}
	}

	resolved := map[string]interface{}{}
	var files []FileInput
	for _, name := range m.paramNames() {
		p := m.Params[name]
		raw, given := inputs[name]
		if given && (len(raw) == 0 || string(raw) == "null") {
			given = false
		}
		if !given {
			var v interface{}
			switch {
			case p.Required:
				return nil, nil, fmt.Errorf("parameter %q is required", name)
			case p.Type == "seed":
				v = randomSeed()
			case p.Default != nil && p.Type != "image":
				v = p.Default
			default:
				continue // keep the workflow's own value
			}
			resolved[name] = v
			g[p.Node].Inputs[p.Input] = v
			continue
		}

		if p.Type == "image" {
			var ref struct {
				Upload   string `json:"upload"`
				URL      string `json:"url"`
				Filename string `json:"filename"`
				MIME     string `json:"mime"`
			}
			if err := json.Unmarshal(raw, &ref); err != nil || ref.Upload == "" {
				return nil, nil, fmt.Errorf("parameter %q must be an upload reference {\"upload\": \"<id>\"}", name)
			}
			if ref.URL == "" {
				return nil, nil, fmt.Errorf("parameter %q: upload %s has no download URL (storage not configured?)", name, ref.Upload)
			}
			files = append(files, FileInput{Param: name, Node: p.Node, Input: p.Input, URL: ref.URL, Filename: ref.Filename, MIME: ref.MIME})
			resolved[name] = map[string]string{"upload": ref.Upload}
			continue
		}

		v, err := p.coerce(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		resolved[name] = v
		g[p.Node].Inputs[p.Input] = v
	}
	return resolved, files, nil
}

func (m *Manifest) paramNames() []string {
	names := make([]string, 0, len(m.Params))
	for n := range m.Params {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (p Param) coerce(raw json.RawMessage) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	switch p.Type {
	case "text":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		maxLen := p.MaxLen
		if maxLen <= 0 {
			maxLen = 4000
		}
		if len(s) > maxLen {
			return nil, fmt.Errorf("must be at most %d characters", maxLen)
		}
		return s, nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("must be true or false")
		}
		return b, nil
	case "seed":
		n, ok := v.(json.Number)
		if !ok {
			return nil, fmt.Errorf("must be a number")
		}
		u, ok := new(big.Int).SetString(n.String(), 10)
		if !ok || u.Sign() < 0 || u.BitLen() > 64 {
			return nil, fmt.Errorf("must be an integer between 0 and 2^64-1")
		}
		return json.Number(u.String()), nil
	case "int", "number":
		n, ok := v.(json.Number)
		if !ok {
			return nil, fmt.Errorf("must be a number")
		}
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("must be a number")
		}
		if p.Type == "int" && f != math.Trunc(f) {
			return nil, fmt.Errorf("must be an integer")
		}
		if p.Min != nil && f < *p.Min {
			return nil, fmt.Errorf("must be >= %v", *p.Min)
		}
		if p.Max != nil && f > *p.Max {
			return nil, fmt.Errorf("must be <= %v", *p.Max)
		}
		return n, nil
	case "choice":
		for _, o := range p.Options {
			if fmt.Sprint(o) == fmt.Sprint(v) {
				return o, nil
			}
		}
		return nil, fmt.Errorf("must be one of %v", p.Options)
	}
	return nil, fmt.Errorf("unsupported type %q", p.Type)
}

func randomSeed() json.Number {
	var b [8]byte
	_, _ = rand.Read(b[:])
	// Keep seeds within 2^53 so every client (JS included) can display them exactly.
	return json.Number(fmt.Sprint(binary.BigEndian.Uint64(b[:]) >> 11))
}
