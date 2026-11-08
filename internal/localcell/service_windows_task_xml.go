package localcell

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const windowsTaskXMLLimit = 1 << 20
const windowsTaskNamespace = "http://schemas.microsoft.com/windows/2004/02/mit/task"

var windowsTaskInstallationID = regexp.MustCompile(`^[a-z0-9]{8,32}$`)
var windowsTaskOwnerSID = regexp.MustCompile(`^S-1-5-21-[0-9]+-[0-9]+-[0-9]+-[0-9]+$`)

// No native export has yet established a benign extra setting. An empty table
// deliberately refuses every extra setting; future entries need native evidence.
var windowsTaskBenignDefaults = map[string]string{}

// WindowsLogonTaskName is the only task namespace this adapter can address.
func WindowsLogonTaskName(d ServiceDescriptor) (string, error) {
	if !windowsTaskInstallationID.MatchString(d.InstallationID) {
		return "", windowsTaskError(ErrServiceOwnershipConflict, "inspect:action", 0, "invalid")
	}
	return `\Witself-Cell-` + d.InstallationID, nil
}

// Use the Windows argv quoting rule independently of the build host. Task
// Scheduler passes Arguments to CreateProcess, never to a command interpreter.
func windowsTaskQuote(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	slashes := 0
	for _, r := range value {
		if r == '\\' {
			slashes++
			continue
		}
		if r == '"' {
			out.WriteString(strings.Repeat(`\`, 2*slashes+1))
		} else {
			out.WriteString(strings.Repeat(`\`, slashes))
		}
		slashes = 0
		out.WriteRune(r)
	}
	out.WriteString(strings.Repeat(`\`, 2*slashes))
	out.WriteByte('"')
	return out.String()
}

func windowsTaskAbsolutePath(value string) bool {
	// Only local, drive-qualified paths. UNC, device namespaces, drive-relative
	// paths, alternate streams and parent aliases are not task host identities.
	if len(value) < 3 || ((value[0] < 'A' || value[0] > 'Z') && (value[0] < 'a' || value[0] > 'z')) || value[1] != ':' || value[2] != '\\' || strings.ContainsAny(value[2:], ":/") {
		return false
	}
	for _, part := range strings.Split(value[3:], `\`) {
		if part == "" || part == "." || part == ".." || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
	}
	return true
}

func validateWindowsTaskSpec(d ServiceDescriptor) error {
	if err := validateWindowsTaskValues(append([]string{d.Root, d.UID, d.InstallationID, d.Payload.Host, d.Payload.Generation}, d.Payload.Args...)...); err != nil {
		return err
	}
	if _, err := WindowsLogonTaskName(d); err != nil {
		return err
	}
	if d.Version != 1 || !windowsTaskOwnerSID.MatchString(d.UID) || d.Scope != "user" || d.Runtime != "local" || d.Adapter != "windows-logon-task" ||
		!windowsTaskAbsolutePath(d.Root) || !windowsTaskAbsolutePath(d.Payload.Host) || d.Payload.Generation == "" || d.Revision == 0 || !validServiceDesiredState(d.DesiredState) || d.ResourceProfile != "unconfigured" {
		return windowsTaskError(ErrServiceOwnershipConflict, "inspect:principal", 0, "invalid")
	}
	for _, s := range append([]string{d.Root, d.UID, d.Payload.Host, d.Payload.Generation}, d.Payload.Args...) {
		if !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 || strings.ContainsAny(s, "\ufffe\uffff") {
			return windowsTaskError(ErrServiceOwnershipConflict, "inspect:action", 0, "invalid")
		}
	}
	return nil
}

// Percent references are refused before XML rendering because Scheduler may
// expand Exec values before task-host has a chance to validate its arguments.
func validateWindowsTaskValues(values ...string) error {
	for _, value := range values {
		if strings.Contains(value, "%") {
			return windowsTaskError(ErrServiceOwnershipConflict, "inspect:action", 0, "task_value_percent_reference")
		}
	}
	return nil
}

type windowsTaskRenderValues struct {
	root, executable, generation, arguments, workingDirectory, author, description string
}

func (v windowsTaskRenderValues) validate() error {
	return validateWindowsTaskValues(v.root, v.executable, v.generation, v.arguments, v.workingDirectory, v.author, v.description)
}

func windowsTaskUTF16(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2+len(units)*2)
	out[0], out[1] = 0xff, 0xfe
	for i, unit := range units {
		binary.LittleEndian.PutUint16(out[2+i*2:], unit)
	}
	return out
}

// RenderWindowsLogonTaskXML renders one least-privilege interactive logon task.
// No task-manager default is relied upon for a behavior-changing setting.
func RenderWindowsLogonTaskXML(d ServiceDescriptor) ([]byte, error) {
	if err := validateWindowsTaskSpec(d); err != nil {
		return nil, err
	}
	name, _ := WindowsLogonTaskName(d)
	values := windowsTaskRenderValues{
		root: d.Root, executable: d.Payload.Host, generation: d.Payload.Generation,
		arguments:        strings.Join([]string{"task-host", "--runtime=local", "--root", windowsTaskQuote(d.Root), "--generation", windowsTaskQuote(d.Payload.Generation)}, " "),
		workingDirectory: d.Root, description: "Witself local cell",
	}
	if err := values.validate(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n<Task version=\"1.2\" xmlns=\"" + windowsTaskNamespace + "\">\n")
	leaf := func(name, value string) {
		out.WriteString("<" + name + ">")
		_ = xml.EscapeText(&out, []byte(value))
		out.WriteString("</" + name + ">\n")
	}
	out.WriteString("<RegistrationInfo>\n")
	leaf("Description", values.description)
	if values.author != "" {
		leaf("Author", values.author)
	}
	leaf("URI", name)
	out.WriteString("</RegistrationInfo>\n<Triggers>\n<LogonTrigger>\n")
	leaf("Enabled", "true")
	leaf("UserId", d.UID)
	out.WriteString("</LogonTrigger>\n</Triggers>\n<Principals>\n<Principal id=\"Author\">\n")
	leaf("UserId", d.UID)
	leaf("LogonType", "InteractiveToken")
	leaf("RunLevel", "LeastPrivilege")
	out.WriteString("</Principal>\n</Principals>\n<Settings>\n")
	for _, setting := range [][2]string{
		{"MultipleInstancesPolicy", "IgnoreNew"}, {"DisallowStartIfOnBatteries", "false"}, {"StopIfGoingOnBatteries", "false"},
		{"AllowHardTerminate", "false"}, {"StartWhenAvailable", "false"}, {"RunOnlyIfNetworkAvailable", "false"},
	} {
		leaf(setting[0], setting[1])
	}
	out.WriteString("<IdleSettings>\n")
	leaf("StopOnIdleEnd", "false")
	leaf("RestartOnIdle", "false")
	out.WriteString("</IdleSettings>\n")
	for _, setting := range [][2]string{
		{"AllowStartOnDemand", "true"}, {"Enabled", "true"}, {"Hidden", "false"}, {"RunOnlyIfIdle", "false"},
		{"WakeToRun", "false"}, {"ExecutionTimeLimit", "PT0S"}, {"Priority", "7"},
	} {
		leaf(setting[0], setting[1])
	}
	out.WriteString("</Settings>\n<Actions Context=\"Author\">\n<Exec>\n")
	leaf("Command", values.executable)
	leaf("Arguments", values.arguments)
	leaf("WorkingDirectory", values.workingDirectory)
	out.WriteString("</Exec>\n</Actions>\n</Task>\n")
	raw := windowsTaskUTF16(out.String())
	if len(raw) > windowsTaskXMLLimit {
		return nil, windowsTaskError(ErrServiceOwnershipConflict, "inspect:action", 0, "too_large")
	}
	return raw, nil
}

// Decode only UTF-8 or BOM-marked UTF-16LE. Invalid surrogates, embedded NULs,
// legacy code pages and BOM-less UTF-16 never reach the XML parser.
func decodeWindowsTaskXML(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > windowsTaskXMLLimit {
		return nil, errors.New("task XML size")
	}
	var decoded []byte
	utf16Input := bytes.HasPrefix(raw, []byte{0xff, 0xfe})
	if utf16Input {
		if len(raw)%2 != 0 {
			return nil, errors.New("task XML encoding")
		}
		units := make([]uint16, (len(raw)-2)/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(raw[2+i*2:])
		}
		for i := 0; i < len(units); i++ {
			if units[i] >= 0xd800 && units[i] <= 0xdbff {
				if i+1 >= len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
					return nil, errors.New("task XML surrogate")
				}
				i++
			} else if units[i] >= 0xdc00 && units[i] <= 0xdfff {
				return nil, errors.New("task XML surrogate")
			}
		}
		decoded = []byte(string(utf16.Decode(units)))
	} else {
		decoded = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
		if !utf8.Valid(decoded) {
			return nil, errors.New("task XML encoding")
		}
	}
	if bytes.IndexByte(decoded, 0) >= 0 {
		return nil, errors.New("task XML NUL")
	}
	// Verify the declaration against the physical encoding before allowing the
	// standard decoder to consume already-decoded bytes.
	declaration := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>"
	if utf16Input {
		declaration = "<?xml version=\"1.0\" encoding=\"UTF-16\"?>"
	}
	if !bytes.HasPrefix(decoded, []byte(declaration)) {
		return nil, errors.New("task XML declaration")
	}
	return bytes.TrimPrefix(decoded, []byte(declaration)), nil
}

type windowsTaskXMLNode struct {
	Name       string
	Attributes map[string]string
	Text       string
	Children   []*windowsTaskXMLNode
}

func parseWindowsTaskXML(raw []byte) (*windowsTaskXMLNode, error) {
	decoded, err := decodeWindowsTaskXML(raw)
	if err != nil {
		return nil, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(decoded))
	var root *windowsTaskXMLNode
	var stack []*windowsTaskXMLNode
	count := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if root == nil || len(stack) != 0 {
				return nil, errors.New("task XML incomplete")
			}
			return root, nil
		}
		if err != nil {
			return nil, errors.New("task XML malformed")
		}
		switch token := token.(type) {
		case xml.StartElement:
			count++
			if token.Name.Space != windowsTaskNamespace || len(stack) >= 8 || count > 128 {
				return nil, errors.New("task XML namespace or bounds")
			}
			node := &windowsTaskXMLNode{Name: token.Name.Local, Attributes: make(map[string]string)}
			for _, attr := range token.Attr {
				if attr.Name.Space != "" {
					return nil, errors.New("task XML namespaced attribute")
				}
				if _, exists := node.Attributes[attr.Name.Local]; exists {
					return nil, errors.New("task XML duplicate attribute")
				}
				node.Attributes[attr.Name.Local] = attr.Value
			}
			if len(stack) == 0 {
				if root != nil || node.Name != "Task" {
					return nil, errors.New("task XML root")
				}
				root = node
			} else {
				parent := stack[len(stack)-1]
				if strings.TrimSpace(parent.Text) != "" {
					return nil, errors.New("task XML mixed content")
				}
				parent.Text = ""
				for _, child := range parent.Children {
					if child.Name == node.Name {
						return nil, errors.New("task XML duplicate singleton")
					}
				}
				parent.Children = append(parent.Children, node)
			}
			stack = append(stack, node)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errors.New("task XML end")
			}
			node := stack[len(stack)-1]
			if len(node.Children) > 0 {
				if strings.TrimSpace(node.Text) != "" {
					return nil, errors.New("task XML mixed content")
				}
				node.Text = ""
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 {
				if strings.TrimSpace(string(token)) != "" {
					return nil, errors.New("task XML trailing text")
				}
				continue
			}
			stack[len(stack)-1].Text += string(token)
		default:
			// In particular: directives (including DTDs), processing instructions,
			// comments and external entities have no place in this owned template.
			return nil, errors.New("task XML unsupported token")
		}
	}
}

// ValidateWindowsLogonTaskXML uses the strict parser as the only equality
// authority. Whitespace between elements and numeric references are equivalent;
// an unlisted field, attribute, principal, trigger, action or setting is not.
func ValidateWindowsLogonTaskXML(raw []byte, d ServiceDescriptor) error {
	wantRaw, err := RenderWindowsLogonTaskXML(d)
	if err != nil {
		return err
	}
	want, err := parseWindowsTaskXML(wantRaw)
	if err != nil {
		return windowsTaskError(ErrServiceOwnershipConflict, "query:result", 0, "template_invalid")
	}
	got, err := parseWindowsTaskXML(raw)
	if err != nil {
		return windowsTaskError(ErrServiceOwnershipConflict, "query:result", 0, "xml_invalid")
	}
	// Compare each behavior-bearing subtree at its fixed R8 raising site. The
	// complete-tree comparison afterwards also refuses unknown top-level data.
	if !reflect.DeepEqual(windowsTaskXMLChild(want, "Principals"), windowsTaskXMLChild(got, "Principals")) {
		return windowsTaskError(ErrServiceOwnershipConflict, "inspect:principal", 0, "mismatch")
	}
	if !reflect.DeepEqual(windowsTaskXMLChild(want, "Triggers"), windowsTaskXMLChild(got, "Triggers")) {
		return windowsTaskError(ErrServiceOwnershipConflict, "inspect:trigger", 0, "mismatch")
	}
	if !reflect.DeepEqual(windowsTaskXMLChild(want, "Actions"), windowsTaskXMLChild(got, "Actions")) {
		return windowsTaskError(ErrServiceOwnershipConflict, "inspect:action", 0, "mismatch")
	}
	if !reflect.DeepEqual(windowsTaskXMLChild(want, "Settings"), windowsTaskXMLChild(got, "Settings")) {
		return windowsTaskError(ErrServiceOwnershipConflict, "inspect:settings", 0, "mismatch")
	}
	if !reflect.DeepEqual(want, got) {
		return windowsTaskError(ErrServiceOwnershipConflict, "query:result", 0, "mismatch")
	}
	return nil
}

func windowsTaskXMLChild(root *windowsTaskXMLNode, name string) *windowsTaskXMLNode {
	for _, child := range root.Children {
		if child.Name == name {
			return child
		}
	}
	return nil
}
