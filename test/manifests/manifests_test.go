/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package manifests validates every pgop manifest shipped with the project
// (config/samples, examples/ and the YAML snippets in the Markdown docs)
// against the generated CRDs, with a server-side dry-run create on an envtest
// API server. That runs the OpenAPI schema, the CEL rules and strict field
// validation (unknown fields are errors), exactly as kubectl apply would.
//
// Markdown conventions (an HTML comment on the line before a fence applies to
// that fence only):
//
//	<!-- pgop-validate: skip -->       not a manifest (schema notation, Secret data, ...)
//	<!-- pgop-validate: invalid -->    the snippet must be rejected by the API server
//	<!-- pgop-validate: kind=Role -->  a fragment of that kind (overrides the file default)
//
// A YAML document without apiVersion is a fragment: its top-level keys are
// either spec (and metadata) or fields of the spec of the file's default kind
// (roles.md: Role, databases.md: Database, ...). Fragments are wrapped into a
// full object, with a clusterRef added where the kind requires one. Heredocs
// (<<EOF ... EOF) in shell blocks are validated as YAML too.
package manifests

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

const (
	pgopGroup     = "pgop.ruck.io"
	testNamespace = "default"
	repoRoot      = "../.."
)

// defaultKinds is the kind of fragments in each user-guide page.
var defaultKinds = map[string]string{
	"roles.md":       "Role",
	"databases.md":   "Database",
	"clusters.md":    "Cluster",
	"replication.md": "Cluster",
	"backups.md":     "Backup",
	"restores.md":    "Restore",
}

// kindsWithClusterRef are the kinds whose fragments get a clusterRef added.
var kindsWithClusterRef = []string{"Role", "Database"}

type snippet struct {
	file          string
	line          int
	kind          string // default kind for fragments, "" for none
	body          string
	skip          bool
	expectInvalid bool
}

func (s snippet) name() string { return fmt.Sprintf("%s:%d", s.file, s.line) }

var (
	directiveRe = regexp.MustCompile(`^\s*<!--\s*pgop-validate:\s*(.*?)\s*-->\s*$`)
	heredocRe   = regexp.MustCompile(`<<-?\s*'?"?([A-Z]+)'?"?`)
)

func TestManifests(t *testing.T) {
	snippets := collect(t)
	if len(snippets) == 0 {
		t.Fatal("no manifests found")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		env.BinaryAssetsDirectory = firstEnvtestBinaryDir()
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	specFields, err := loadSpecFields(filepath.Join(repoRoot, "config", "crd", "bases"))
	if err != nil {
		t.Fatalf("reading CRDs: %v", err)
	}

	var validated, skipped int
	for _, s := range snippets {
		if s.skip {
			skipped++
			continue
		}
		t.Run(s.name(), func(t *testing.T) {
			validated += validateSnippet(t, c, specFields, s)
		})
	}
	t.Logf("validated %d objects from %d snippets (%d skipped by directive)", validated, len(snippets)-skipped, skipped)
}

// validateSnippet validates every document of s and returns how many objects
// it sent to the API server.
func validateSnippet(t *testing.T, c client.Client, specFields map[string][]string, s snippet) int {
	docs, err := splitYAML(s.body)
	if err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	n := 0
	for i, doc := range docs {
		obj, why := toObject(doc, s.kind, specFields)
		if obj == nil {
			if why != "" {
				t.Errorf("document %d: %s; add <!-- pgop-validate: skip --> if it is not a manifest", i, why)
			}
			continue
		}
		n++
		obj.SetNamespace(testNamespace)
		err := c.Create(context.Background(), obj, client.DryRunAll, client.FieldValidation("Strict"))
		switch {
		case s.expectInvalid && err == nil:
			t.Errorf("document %d (%s %s): expected the API server to reject it", i, obj.GetKind(), obj.GetName())
		case !s.expectInvalid && err != nil:
			t.Errorf("document %d (%s %s): %v", i, obj.GetKind(), obj.GetName(), err)
		}
	}
	if n == 0 && s.expectInvalid {
		t.Error("marked invalid but contains no manifest")
	}
	return n
}

// toObject turns a YAML document into an object to validate. It returns nil
// and an empty reason for documents that are deliberately not validated
// (empty documents, kustomizations, kinds the envtest server does not know),
// and nil with a reason for documents it cannot classify.
func toObject(doc map[string]any, kind string, specFields map[string][]string) (*unstructured.Unstructured, string) {
	if len(doc) == 0 {
		return nil, ""
	}
	if apiVersion, ok := doc["apiVersion"].(string); ok {
		group, _, _ := strings.Cut(apiVersion, "/")
		if group != pgopGroup && apiVersion != "v1" {
			return nil, "" // cert-manager, kustomize, apps: not served by envtest or not ours
		}
		obj := &unstructured.Unstructured{Object: doc}
		if obj.GetName() == "" && obj.GetGenerateName() == "" {
			obj.SetName("doc-snippet")
		}
		return obj, ""
	}
	if kind == "" {
		return nil, fmt.Sprintf("fragment without apiVersion (keys %v) and no default kind", keys(doc))
	}
	spec, why := fragmentSpec(doc, kind, specFields[kind])
	if spec == nil {
		return nil, why
	}
	if slices.Contains(kindsWithClusterRef, kind) {
		if _, ok := spec["clusterRef"]; !ok {
			spec["clusterRef"] = map[string]any{"name": "doc-cluster"}
		}
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": pgopGroup + "/v1alpha1",
		"kind":       kind,
		"metadata":   map[string]any{"name": "doc-snippet"},
		"spec":       spec,
	}}
	if md, ok := doc["metadata"].(map[string]any); ok {
		obj.Object["metadata"] = md
	}
	return obj, ""
}

func fragmentSpec(doc map[string]any, kind string, fields []string) (map[string]any, string) {
	ks := keys(doc)
	if spec, ok := doc["spec"].(map[string]any); ok {
		for _, k := range ks {
			if k != "spec" && k != "metadata" {
				return nil, fmt.Sprintf("%s fragment has keys besides spec/metadata: %v", kind, ks)
			}
		}
		return spec, ""
	}
	for _, k := range ks {
		if !slices.Contains(fields, k) {
			return nil, fmt.Sprintf("%s fragment: %q is not a spec field (keys %v)", kind, k, ks)
		}
	}
	return doc, ""
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func splitYAML(body string) ([]map[string]any, error) {
	r := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(body)))
	var out []map[string]any
	for {
		raw, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
}

// loadSpecFields returns the top-level spec properties of each pgop kind.
func loadSpecFields(dir string) (map[string][]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, v := range crd.Spec.Versions {
			if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
				continue
			}
			for k := range v.Schema.OpenAPIV3Schema.Properties["spec"].Properties {
				out[crd.Spec.Names.Kind] = append(out[crd.Spec.Names.Kind], k)
			}
		}
	}
	return out, nil
}

// collect finds the manifests and Markdown snippets to validate.
func collect(t *testing.T) []snippet {
	t.Helper()
	var out []snippet
	for _, dir := range []string{"config/samples", "examples"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return filepath.SkipDir
				}
				return err
			}
			if d.IsDir() || d.Name() == "kustomization.yaml" ||
				(filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out = append(out, snippet{file: rel(path), line: 1, body: string(raw)})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	mdFiles := []string{filepath.Join(repoRoot, "README.md")}
	err := filepath.WalkDir(filepath.Join(repoRoot, "docs"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".md" {
			mdFiles = append(mdFiles, path)
		}
		return err
	})
	if err != nil {
		t.Fatalf("walking docs: %v", err)
	}
	for _, f := range mdFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		out = append(out, markdownSnippets(rel(f), defaultKinds[filepath.Base(f)], raw)...)
	}
	return out
}

func rel(path string) string {
	r, err := filepath.Rel(repoRoot, path)
	if err != nil {
		return path
	}
	return r
}

// markdownSnippets returns the YAML fences of a Markdown file, and the
// heredocs of its shell fences.
func markdownSnippets(file, kind string, raw []byte) []snippet {
	lines := strings.Split(string(raw), "\n")
	var out []snippet
	directive := ""
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := directiveRe.FindStringSubmatch(line); m != nil {
			directive = m[1]
			continue
		}
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "```") {
			if trimmed != "" {
				directive = ""
			}
			continue
		}
		lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
		indent := len(line) - len(strings.TrimLeft(line, " "))
		start := i + 1
		end := start
		for end < len(lines) && strings.TrimSpace(lines[end]) != "```" {
			end++
		}
		body := dedent(lines[start:end], indent)
		i = end
		switch lang {
		case "yaml", "yml":
			out = append(out, newSnippet(file, start, kind, body, directive))
		case "sh", "bash", "shell", "console":
			for _, h := range heredocs(body, start) {
				out = append(out, newSnippet(file, h.line, kind, h.body, directive))
			}
		}
		directive = ""
	}
	return out
}

func newSnippet(file string, line int, kind, body, directive string) snippet {
	s := snippet{file: file, line: line, kind: kind, body: body}
	switch {
	case directive == "skip" || strings.HasPrefix(directive, "skip "):
		s.skip = true
	case directive == "invalid":
		s.expectInvalid = true
	case strings.HasPrefix(directive, "kind="):
		s.kind = strings.TrimPrefix(directive, "kind=")
	}
	return s
}

func dedent(lines []string, indent int) string {
	var b bytes.Buffer
	for _, l := range lines {
		if len(l) >= indent && strings.TrimSpace(l[:indent]) == "" {
			l = l[indent:]
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

type heredoc struct {
	line int
	body string
}

func heredocs(body string, firstLine int) []heredoc {
	lines := strings.Split(body, "\n")
	var out []heredoc
	for i := 0; i < len(lines); i++ {
		m := heredocRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		start := i + 1
		end := start
		for end < len(lines) && strings.TrimSpace(lines[end]) != m[1] {
			end++
		}
		out = append(out, heredoc{line: firstLine + start, body: strings.Join(lines[start:end], "\n")})
		i = end
	}
	return out
}

// firstEnvtestBinaryDir finds envtest binaries installed by make setup-envtest
// when KUBEBUILDER_ASSETS is not set (for example when run from an IDE).
func firstEnvtestBinaryDir() string {
	base := filepath.Join(repoRoot, "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}
