package wiresnapshot

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const SnapshotPath = "internal/wiresnapshot/wire-surface.txt"

const RegenerateCommand = "make wire-snapshot"

type constRule int

const (
	constsNone constRule = iota
	constsExportedStrings
	constsExported
)

type typeRule struct {
	jsonTagged bool
	named      []string
}

type wireSection struct {
	title      string
	why        []string
	dir        string
	rule       typeRule
	constFiles []string
	consts     constRule
}

var wireSections = []wireSection{
	{
		title: "control-plane HTTP — controlplane (types and vocabulary; routes are the private repo's golden)",
		why: []string{
			"What every laptop, box and conductor calls, and what the control plane",
			"answers. A client older than the deployment decodes these structs. This",
			"is the wire with a floor and a refusal; the other three have neither. The",
			"constants pinned here are the string vocabulary an old client hardcoded",
			"(kinds, roles, states, headers); the numbers are the deployment's own",
			"policy and are deliberately not pinned — and neither are the ROUTES,",
			"which are service code and are the private repo's golden.",
		},
		dir:    "controlplane",
		rule:   typeRule{jsonTagged: true},
		consts: constsExportedStrings,
	},
	{
		title: "session network framing — sessionnet",
		why: []string{
			"Two clients, spliced through a relay that carries ciphertext only. No",
			"version check exists here and none can: the relay never reads the",
			"control floor, so a framing change is not refused, it is GARBLED —",
			"unsurvivable and mute, mid-clone. The record layer's own struct is",
			"pinned because the length-prefix width and the nonce counters live in",
			"its fields.",
		},
		dir:        "sessionnet",
		rule:       typeRule{named: []string{"Key", "secureConn"}},
		constFiles: []string{"conn.go"},
		consts:     constsExported,
	},
	{
		title: "telemetry envelope — telemetry",
		why: []string{
			"The shape a client, relay or control plane POSTs to the ingest (a JSON",
			"array of these). The ingest drops what it cannot read without ceremony",
			"and nothing rides back to the client, so a shape change is silent data",
			"loss rather than an error anybody sees.",
		},
		dir:  "telemetry",
		rule: typeRule{jsonTagged: true},
	},
	{
		title: "relay ticket claims — relayticket",
		why: []string{
			"Minted by the control worker, verified offline by the session worker,",
			"cached client-side for up to TicketTTL. Three parties hold a ticket at",
			"once, so a format change garbles all three. FIELD ORDER IS THE WIRE:",
			"the payload is the fields NUL-separated in declaration order, followed",
			"by an 8-byte big-endian expiry.",
		},
		dir:    "relayticket",
		rule:   typeRule{named: []string{"Claims"}},
		consts: constsExported,
	},
}

func GenerateWireSurface(root string) (string, error) {
	var b strings.Builder
	b.WriteString(header)
	for _, section := range wireSections {
		pkg, err := parsePackage(filepath.Join(root, section.dir))
		if err != nil {
			return "", err
		}
		if err := renderSection(&b, section, pkg); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

func LoadWireSurface(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, SnapshotPath))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

const header = `# club wire surface — GENERATED. Do not edit by hand.
#
# Regenerate:  make wire-snapshot
# Pinned by:   go test ./internal/wiresnapshot/ -run TestWireSurfaceMatchesTheCommittedSnapshot
# Classify:    .wire-changes/README.md
#
# This file exists so that a change to a shape somebody else's already-installed
# club can see cannot land unclassified. Drift here is red, and the failure
# text says what to write. Field order is declaration order; everything else is
# sorted. The tripwire is SHAPE-ONLY: a behaviour change that breaks an old
# client without moving a shape is still yours to classify unprompted.
#
# The control plane's ROUTE TABLE is not here. It is service code and lives in
# the private services repo, which keeps a routes-only golden of its own — a
# route added or dropped is red over there, in the repo that made the change.

`

func renderSection(b *strings.Builder, section wireSection, pkg *parsedPackage) error {
	if missing := pkg.missingNamedTypes(section); len(missing) > 0 {
		return fmt.Errorf("wiresnapshot: %s no longer declares %s — it was renamed or removed. "+
			"Point wireSections at the type that replaced it, or drop the section if that wire is gone",
			section.dir, strings.Join(missing, ", "))
	}
	fmt.Fprintf(b, "## %s\n", section.title)
	for _, line := range section.why {
		fmt.Fprintf(b, "# %s\n", line)
	}
	b.WriteString("\n")

	for _, c := range pkg.selectConsts(section) {
		fmt.Fprintf(b, "const %s = %s\n", c.name, c.value)
	}

	for _, t := range pkg.selectTypes(section) {
		if !t.isStruct {
			fmt.Fprintf(b, "type %s %s\n", t.name, t.underlying)
			continue
		}
		fmt.Fprintf(b, "type %s struct\n", t.name)
		for _, f := range t.fields {
			switch {
			case f.tag != "":
				fmt.Fprintf(b, "\t%s %s `%s`\n", f.name, f.typ, f.tag)
			case f.name == f.typ:
				fmt.Fprintf(b, "\t%s\n", f.typ)
			default:
				fmt.Fprintf(b, "\t%s %s\n", f.name, f.typ)
			}
		}
	}
	b.WriteString("\n")
	return nil
}

type parsedField struct{ name, typ, tag string }

type parsedType struct {
	name       string
	isStruct   bool
	underlying string
	fields     []parsedField
	jsonTagged bool
}

type parsedConst struct {
	name, value, file string
	isString          bool
}

type parsedPackage struct {
	dir    string
	types  []parsedType
	consts []parsedConst
}

func parsePackage(dir string) (*parsedPackage, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("wiresnapshot: reading %s: %w", dir, err)
	}
	pkg := &parsedPackage{dir: dir}
	fset := token.NewFileSet()
	var names []string
	for _, item := range items {
		name := item.Name()
		if item.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("wiresnapshot: parsing %s: %w", filepath.Join(dir, name), err)
		}
		pkg.readFile(name, file)
	}
	return pkg, nil
}

func (p *parsedPackage) readFile(name string, file *ast.File) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		switch gen.Tok {
		case token.TYPE:
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				p.types = append(p.types, readType(ts))
			}
		case token.CONST:
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range vs.Names {
					value := "(inherited)"
					isString := false
					if i < len(vs.Values) {
						value = types.ExprString(vs.Values[i])
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							isString = true
						}
					}
					p.consts = append(p.consts, parsedConst{name: ident.Name, value: value, file: name, isString: isString})
				}
			}
		}
	}
}

func readType(ts *ast.TypeSpec) parsedType {
	out := parsedType{name: ts.Name.Name}
	st, ok := ts.Type.(*ast.StructType)
	if !ok {
		out.underlying = types.ExprString(ts.Type)
		return out
	}
	out.isStruct = true
	for _, field := range st.Fields.List {
		tag := ""
		if field.Tag != nil {
			if unquoted, err := strconv.Unquote(field.Tag.Value); err == nil {
				tag = unquoted
			}
		}
		typ := types.ExprString(field.Type)
		if len(field.Names) == 0 {
			out.fields = append(out.fields, parsedField{name: typ, typ: typ, tag: tag})
			continue
		}
		for _, ident := range field.Names {
			out.fields = append(out.fields, parsedField{name: ident.Name, typ: typ, tag: tag})
		}
	}
	for _, f := range out.fields {
		if jsonName(f) != "" {
			out.jsonTagged = true
		}
	}
	return out
}

func jsonName(f parsedField) string {
	if !ast.IsExported(f.name) {
		return ""
	}
	tag, ok := lookupTag(f.tag, "json")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return ""
	}
	if name == "" {
		return f.name
	}
	return name
}

func lookupTag(tag, key string) (string, bool) {
	for _, part := range strings.Fields(tag) {
		k, v, ok := strings.Cut(part, ":")
		if !ok || k != key {
			continue
		}
		unquoted, err := strconv.Unquote(v)
		if err != nil {
			return "", false
		}
		return unquoted, true
	}
	return "", false
}

func (p *parsedPackage) selectTypes(section wireSection) []parsedType {
	named := map[string]bool{}
	for _, name := range section.rule.named {
		named[name] = true
	}
	var out []parsedType
	for _, t := range p.types {
		wanted := named[t.name] ||
			(section.rule.jsonTagged && ast.IsExported(t.name) && t.jsonTagged)
		if !wanted {
			continue
		}
		if section.rule.jsonTagged {
			var wire []parsedField
			for _, f := range t.fields {
				if jsonName(f) == "" {
					continue
				}
				wire = append(wire, f)
			}
			t.fields = wire
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func (p *parsedPackage) missingNamedTypes(section wireSection) []string {
	have := map[string]bool{}
	for _, t := range p.types {
		have[t.name] = true
	}
	var missing []string
	for _, name := range section.rule.named {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func (p *parsedPackage) selectConsts(section wireSection) []parsedConst {
	files := map[string]bool{}
	for _, name := range section.constFiles {
		files[name] = true
	}
	var out []parsedConst
	for _, c := range p.consts {
		exported := ast.IsExported(c.name)
		wanted := files[c.file] ||
			(section.consts == constsExported && exported) ||
			(section.consts == constsExportedStrings && exported && c.isString)
		if wanted {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
