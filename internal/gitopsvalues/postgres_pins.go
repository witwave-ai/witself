package gitopsvalues

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// PostgresImagePins selects the PostgreSQL server image, separately from its
// backup image. Existing upstream pins may omit tag when a digest is present.
type PostgresImagePins struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string

	allowInsecureImages    bool
	allowInsecureImagesSet bool
}

var postgresRegistryPattern = regexp.MustCompile(`^[a-z0-9]+([.-][a-z0-9]+)*(:[0-9]+)?$`)
var postgresRepositoryPattern = regexp.MustCompile(`^[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*(/[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*)*$`)

func (p PostgresImagePins) validate() error {
	if p.Registry == "" && p.Repository == "" && p.Tag == "" && p.Digest == "" {
		return nil
	}
	if !postgresRegistryPattern.MatchString(p.Registry) || !postgresRepositoryPattern.MatchString(p.Repository) || (p.Tag == "" && p.Digest == "") {
		return fmt.Errorf("apps.civoPostgres.image requires a valid registry, repository, and tag or digest")
	}
	if p.Tag != "" && !backupTagPattern.MatchString(p.Tag) {
		return fmt.Errorf("apps.civoPostgres.image.tag must be a valid image tag")
	}
	if p.Digest != "" && !imageDigestPattern.MatchString(p.Digest) {
		return fmt.Errorf("apps.civoPostgres.image.digest must be empty or sha256 followed by 64 lowercase hexadecimal characters")
	}
	return nil
}

func readPostgresImagePins(path string) (*PostgresImagePins, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pins, err := parsePostgresImagePins(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return pins, nil
}

func parsePostgresImagePins(body []byte) (*PostgresImagePins, error) {
	var doc struct {
		Apps struct {
			CivoPostgres struct {
				Image yaml.Node `yaml:"image"`
				Allow yaml.Node `yaml:"allowInsecureImages"`
			} `yaml:"civoPostgres"`
		} `yaml:"apps"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse cell values: %w", err)
	}
	pg := doc.Apps.CivoPostgres
	pins := &PostgresImagePins{}
	if pg.Allow.Kind != 0 {
		if pg.Allow.Kind != yaml.ScalarNode || pg.Allow.Tag != "!!bool" {
			return nil, fmt.Errorf("apps.civoPostgres.allowInsecureImages must be a boolean")
		}
		if err := pg.Allow.Decode(&pins.allowInsecureImages); err != nil {
			return nil, fmt.Errorf("apps.civoPostgres.allowInsecureImages must be a boolean")
		}
		pins.allowInsecureImagesSet = true
	}
	if pg.Image.Kind != 0 {
		if pg.Image.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("apps.civoPostgres.image must be an image pin object")
		}
		fields := map[string]*string{"registry": &pins.Registry, "repository": &pins.Repository, "tag": &pins.Tag, "digest": &pins.Digest}
		for i := 0; i+1 < len(pg.Image.Content); i += 2 {
			key, value := pg.Image.Content[i], pg.Image.Content[i+1]
			field, ok := fields[key.Value]
			if !ok || key.Tag != "!!str" {
				return nil, fmt.Errorf("apps.civoPostgres.image contains an unknown or duplicate field")
			}
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return nil, fmt.Errorf("apps.civoPostgres.image.%s must be a string", key.Value)
			}
			*field = value.Value
			delete(fields, key.Value)
		}
	}
	if err := pins.validate(); err != nil {
		return nil, err
	}
	if pins.Registry == "" && !pins.allowInsecureImagesSet {
		return nil, nil
	}
	return pins, nil
}

// addPostgresImagePins replaces only the image selection and explicit child-chart
// verification opt-in. The overlay owns the digest; mismatched content is drift.
// Frozen overlay comments and unrelated bytes are preserved.
// When the existing pins match the template, the original bytes pass through.
func addPostgresImagePins(body []byte, pins *PostgresImagePins) ([]byte, error) {
	if pins == nil {
		return body, nil
	}
	if err := pins.validate(); err != nil {
		return nil, err
	}
	existing, err := parsePostgresImagePins(body)
	if err != nil {
		return nil, err
	}
	if pins.Registry == "" && existing != nil {
		// An explicit child-chart verification setting is independent of image
		// selection; an unset image must still inherit the frozen template pins.
		selected := *pins
		selected.Registry, selected.Repository = existing.Registry, existing.Repository
		selected.Tag, selected.Digest = existing.Tag, existing.Digest
		pins = &selected
	}
	if pins.Registry != "" && (existing == nil || existing.Digest != pins.Digest) {
		return nil, fmt.Errorf("PostgreSQL image digest drift: persisted or override digest differs from the overlay; use --write to regenerate the overlay digest before rolling")
	}
	if existing != nil && *existing == *pins {
		return body, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil || len(doc.Content) != 1 {
		return nil, fmt.Errorf("generated cell values must contain one YAML document")
	}
	_, apps := postgresMappingField(doc.Content[0], "apps")
	_, pg := postgresMappingField(apps, "civoPostgres")
	_, enabled := postgresMappingField(pg, "enabled")
	if enabled == nil || enabled.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("generated cell values lack apps.civoPostgres.enabled; cannot pin PostgreSQL image")
	}
	lines := strings.SplitAfter(string(body), "\n")
	line := lines[enabled.Line-1]
	indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
	imageKey, imageNode := postgresMappingField(pg, "image")
	allowKey, _ := postgresMappingField(pg, "allowInsecureImages")
	type replacement struct {
		start, end int
		body       string
	}
	var changes []replacement
	var image string
	if pins.Registry != "" {
		image = indent + "image:\n" + indent + "  registry: " + strconv.Quote(pins.Registry) + "\n" + indent + "  repository: " + strconv.Quote(pins.Repository) + "\n"
		if pins.Tag != "" {
			image += indent + "  tag: " + strconv.Quote(pins.Tag) + "\n"
		}
		if pins.Digest != "" {
			image += indent + "  digest: " + pins.Digest + "\n"
		}
	}
	if pins.allowInsecureImagesSet {
		allow := indent + "allowInsecureImages: " + strconv.FormatBool(pins.allowInsecureImages) + "\n"
		if allowKey == nil {
			image = allow + image
		} else {
			changes = append(changes, replacement{allowKey.Line - 1, allowKey.Line, allow})
		}
	} else if allowKey != nil {
		changes = append(changes, replacement{allowKey.Line - 1, allowKey.Line, ""})
	}
	if imageKey == nil {
		changes = append(changes, replacement{enabled.Line, enabled.Line, image})
	} else {
		end := imageKey.Line
		for _, node := range imageNode.Content {
			if node.Line > end {
				end = node.Line
			}
		}
		image = preservePostgresImageComments(lines[imageKey.Line-1:end], image, imageKey, imageNode)
		changes = append(changes, replacement{imageKey.Line - 1, end, image})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].start > changes[j].start })
	for _, change := range changes {
		replaced := append([]string{}, lines[:change.start]...)
		replaced = append(replaced, change.body)
		lines = append(replaced, lines[change.end:]...)
	}
	return []byte(strings.Join(lines, "")), nil
}

// preservePostgresImageComments keeps comments on and between existing image
// fields attached to those fields when their values are rewritten. New fields
// retain their canonical position without dropping standalone comment lines.
func preservePostgresImageComments(before []string, after string, imageKey, imageNode *yaml.Node) string {
	if len(before) == 0 || imageNode.Style&yaml.FlowStyle != 0 {
		return after
	}
	comments := make(map[string]string)
	inline := make(map[string]string)
	fieldAtLine := make(map[int]string)
	for i := 0; i+1 < len(imageNode.Content); i += 2 {
		key, value := imageNode.Content[i], imageNode.Content[i+1]
		fieldAtLine[key.Line-imageKey.Line] = key.Value
		inline[key.Value] = value.LineComment
	}
	var pending strings.Builder
	for i, line := range before[1:] {
		if field, ok := fieldAtLine[i+1]; ok {
			comments[field] = pending.String()
			pending.Reset()
		} else if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			pending.WriteString(line)
		}
	}
	var out strings.Builder
	for _, line := range strings.SplitAfter(after, "\n") {
		field, _, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			out.WriteString(line)
			continue
		}
		out.WriteString(comments[field])
		delete(comments, field)
		comment := inline[field]
		if field == "image" {
			comment = imageKey.LineComment
		}
		if comment != "" {
			line = strings.TrimSuffix(line, "\n") + " " + comment + "\n"
		}
		out.WriteString(line)
	}
	// A removed optional field must not remove its surrounding explanation.
	for _, field := range []string{"registry", "repository", "tag", "digest"} {
		if comment, ok := comments[field]; ok {
			out.WriteString(comment)
			if inline[field] != "" {
				out.WriteString(strings.Repeat(" ", imageKey.Column+1) + inline[field] + "\n")
			}
		}
	}
	out.WriteString(pending.String())
	return out.String()
}

func postgresMappingField(node *yaml.Node, name string) (*yaml.Node, *yaml.Node) {
	if node != nil && node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == name {
				return node.Content[i], node.Content[i+1]
			}
		}
	}
	return nil, nil
}
