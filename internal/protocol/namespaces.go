package protocol

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
)

// toolIdentity is protocol-only metadata. The Chat wire sees only chatName.
// Namespace descriptions have no equivalent in Chat, so this subset requires
// an explicitly empty description rather than injecting it into child prompts.
type toolIdentity struct {
	name      string
	namespace string
}

type toolIndex struct {
	// Only current declarations participate in output validation and choice.
	byChat     map[string]toolIdentity
	byIdentity map[toolIdentity]string
	byLocal    map[string][]string
	// All observed identities reserve wire names, including retired history.
	// Recording history here must never enable it as a callable tool.
	wireOwners map[string]toolIdentity
}

func newToolIndex() *toolIndex {
	return &toolIndex{byChat: map[string]toolIdentity{}, byIdentity: map[toolIdentity]string{}, byLocal: map[string][]string{}, wireOwners: map[string]toolIdentity{}}
}

// Short names retain the original length-delimited encoding. Long qualified
// names use a domain-separated full SHA-256 digest, base64url encoded to fit the
// Chat limit. Reverse translation uses the identity index, never decodes a hash.
// Both forms are stable across requests and independent of declaration order.
func namespaceChatName(namespace, name, path string) (string, error) {
	for _, s := range []string{namespace, name} {
		if len(s) == 0 || len(s) > 64 {
			return "", invalid(path, "namespace and child names must be 1..64 ASCII name characters")
		}
		for _, c := range []byte(s) {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
				return "", invalid(path, "namespace and child names must use letters, digits, underscores or hyphens")
			}
		}
	}
	alias := "ns_" + strconv.Itoa(len(namespace)) + "_" + namespace + "_" + name
	if len(alias) > 64 {
		sum := sha256.Sum256([]byte("wb2a:namespace:v1:" + alias))
		alias = "nsh_" + base64.RawURLEncoding.EncodeToString(sum[:])
	}
	return alias, nil
}

// claim checks collisions even between current and historical identities. Same
// identity may recur in history; distinct identities may never share a wire name.
func (idx *toolIndex) claim(id toolIdentity, path string) (string, error) {
	alias := id.name
	if id.namespace != "" {
		var err error
		alias, err = namespaceChatName(id.namespace, id.name, path)
		if err != nil {
			return "", err
		}
	}
	if owner, exists := idx.wireOwners[alias]; exists && owner != id {
		return "", invalid(path, "colliding Chat tool identities")
	}
	idx.wireOwners[alias] = id
	return alias, nil
}

func (idx *toolIndex) add(m map[string]any, namespace, path string, dropped *[]string) (map[string]any, error) {
	ignored, _ := fieldsAccept(m, path, "type name description parameters strict", "*")
	if dropped != nil {
		for _, k := range ignored {
			*dropped = append(*dropped, path+"."+k)
		}
	}
	if m["type"] != "function" {
		return nil, invalid(path+".type", "only client function tools are supported")
	}
	if v, ok := m["strict"].(bool); ok && v {
		if dropped != nil {
			*dropped = append(*dropped, path+".strict")
		}
	}
	t, err := function(m["name"], m["description"], m["parameters"], path)
	if err != nil {
		return nil, err
	}
	fn := t["function"].(map[string]any)
	id := toolIdentity{name: fn["name"].(string), namespace: namespace}
	if _, exists := idx.byIdentity[id]; exists {
		return nil, invalid(path+".name", "duplicate tool declaration")
	}
	alias, err := idx.claim(id, path+".name")
	if err != nil {
		return nil, err
	}
	idx.byChat[alias], idx.byIdentity[id] = id, alias
	idx.byLocal[id.name] = append(idx.byLocal[id.name], alias)
	fn["name"] = alias
	return t, nil
}

func (idx *toolIndex) declarations(v any, dropped *[]string) ([]any, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, invalid("tools", "array required")
	}
	out := make([]any, 0, len(a))
	namespaces := map[string]bool{}
	for i, v := range a {
		p := fmt.Sprintf("tools[%d]", i)
		m, err := object(v, p)
		if err != nil {
			return nil, err
		}
		if m["type"] != "namespace" {
			t, err := idx.add(m, "", p, dropped)
			if err != nil {
				return nil, err
			}
			out = append(out, t)
			continue
		}
		ignored, _ := fieldsAccept(m, p, "type name description tools", "*")
		if dropped != nil {
			for _, k := range ignored {
				*dropped = append(*dropped, p+"."+k)
			}
		}
		ns, err := nonempty(m["name"], p+".name")
		if err != nil {
			return nil, err
		}
		if namespaces[ns] {
			return nil, invalid(p+".name", "duplicate namespace declaration")
		}
		namespaces[ns] = true
		if v := m["description"]; v != nil && v != "" {
			if _, ok := v.(string); !ok {
				return nil, invalid(p+".description", "string required")
			}
			if dropped != nil {
				*dropped = append(*dropped, p+".description")
			}
		}
		children, ok := m["tools"].([]any)
		if !ok || len(children) == 0 {
			return nil, invalid(p+".tools", "non-empty function tool array required")
		}
		for j, child := range children {
			cp := fmt.Sprintf("%s.tools[%d]", p, j)
			m, err := object(child, cp)
			if err != nil {
				return nil, err
			}
			t, err := idx.add(m, ns, cp, dropped)
			if err != nil {
				return nil, err
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// The official function tool_choice has only name (no namespace field). Resolve
// a local name only when it identifies exactly one declaration; ambiguous local
// names must use auto/required or a narrower declaration set, not guessed aliases.
func (idx *toolIndex) choice(v any) (string, error) {
	name, err := nonempty(v, "tool_choice.name")
	if err != nil {
		return "", err
	}
	names := idx.byLocal[name]
	if len(names) != 1 {
		return "", invalid("tool_choice.name", "selected function must identify exactly one declared tool")
	}
	return names[0], nil
}

func (idx *toolIndex) history(name string, namespace any, path string) (string, error) {
	id := toolIdentity{name: name}
	if namespace != nil {
		ns, err := nonempty(namespace, path+".namespace")
		if err != nil {
			return "", err
		}
		id.namespace = ns
	}
	// Explicit namespace identifies namespaced history. Its absence identifies a
	// flat tool, even if this request declares a namespaced tool of the same local
	// name. No declaration is synthesized and no namespace is guessed.
	return idx.claim(id, path+".name")
}

func (req *Request) responseToolIdentity(chatName string) toolIdentity {
	if req.tools != nil {
		if id, ok := req.tools.byChat[chatName]; ok {
			return id
		}
	}
	return toolIdentity{name: chatName}
}
