// bonsai-agent — un agente CLI sobre la API de Bonsai-27B.
//
// El modelo corre 100% en una pestaña del navegador (WebGPU); esta API relaya
// los prompts a esa pestaña. Este binario le pone adelante un loop de
// tool-calling y EJECUTA las herramientas en TU máquina (el modelo solo pide,
// nunca ejecuta nada por su cuenta).
//
// Uso:
//
//	set BONSAI_SECRET=...           (el API_SECRET del deploy)
//	bonsai-agent "¿qué archivos hay acá y de qué trata el proyecto?"   (one-shot)
//	bonsai-agent --chat            (chat interactivo: mantiene la conversación)
//	bonsai-agent --stream "..."    (imprime la respuesta a medida que se genera)
//	bonsai-agent --okf ./kb "..."  (monta un bundle OKF como contexto/memoria)
//	bonsai-agent --yes "corré los tests"   (--yes: no pide confirmación de shell)
//
// Modos: con prompt y sin --chat es one-shot; sin prompt o con --chat/-c entra
// al chat (comandos /exit, /reset, /help). --stream/-s se combina con cualquiera.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// ── tipos de la API ──────────────────────────────────────────────────────────

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type GenRequest struct {
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
	Think    bool      `json:"think,omitempty"`
	Stream   bool      `json:"stream,omitempty"`
}

type ToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type GenResponse struct {
	ID        string     `json:"id"`
	Text      string     `json:"text"`
	Think     string     `json:"think"`
	ToolCalls []ToolCall `json:"tool_calls"`
	Assistant string     `json:"assistant"`
	Warning   string     `json:"warning"`
	Error     string     `json:"error"`
	MS        int        `json:"ms"`
}

// ── herramientas locales ─────────────────────────────────────────────────────

// prop es un par (nombre, descripción) de una propiedad string del schema.
type prop struct{ name, desc string }

// strSchema arma un JSON-Schema de objeto con propiedades string.
func strSchema(required []string, props ...prop) map[string]any {
	p := map[string]any{}
	for _, x := range props {
		p[x.name] = map[string]any{"type": "string", "description": x.desc}
	}
	s := map[string]any{"type": "object", "properties": p}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var tools = []Tool{
	{Type: "function", Function: ToolFunction{
		Name:        "list_dir",
		Description: "Lista archivos y carpetas de un directorio. Si no se pasa path, usa el actual.",
		Parameters:  strSchema(nil, prop{"path", "Ruta del directorio"}),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "read_file",
		Description: "Lee y devuelve el contenido de un archivo de texto.",
		Parameters:  strSchema([]string{"path"}, prop{"path", "Ruta del archivo a leer"}),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "glob",
		Description: "Busca archivos por patrón. Soporta * ? [..] y ** (recursivo). Ej: *.go, src/**/*.js.",
		Parameters:  strSchema([]string{"pattern"}, prop{"pattern", "Patrón glob a buscar"}),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "search",
		Description: "Busca un patrón (regex RE2) dentro de archivos, recursivo. Devuelve archivo:línea: contenido. path opcional (default: el actual).",
		Parameters:  strSchema([]string{"pattern"}, prop{"pattern", "Patrón regex a buscar"}, prop{"path", "Archivo o carpeta donde buscar"}),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "http_get",
		Description: "Hace un GET HTTP y devuelve el status, content-type y el cuerpo (truncado).",
		Parameters:  strSchema([]string{"url"}, prop{"url", "La URL a pedir"}),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "write_file",
		Description: "Escribe (o sobrescribe) un archivo de texto con el contenido dado. El usuario confirma antes.",
		Parameters:  strSchema([]string{"path", "content"}, prop{"path", "Ruta del archivo"}, prop{"content", "Contenido a escribir"}),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "edit_file",
		Description: "Reemplaza UN fragmento exacto en un archivo (old -> new). 'old' debe coincidir literal (con espacios/saltos) y ser único en el archivo. El usuario confirma antes.",
		Parameters:  strSchema([]string{"path", "old", "new"}, prop{"path", "Ruta del archivo"}, prop{"old", "Texto exacto a reemplazar (único en el archivo)"}, prop{"new", "Texto nuevo (vacío = borra el fragmento)"}),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "run_command",
		Description: "Ejecuta un comando de shell en la máquina del usuario y devuelve su salida (stdout+stderr). El usuario confirma antes de correr.",
		Parameters:  strSchema([]string{"command"}, prop{"command", "El comando a ejecutar"}),
	}},
}

// okfDir, si no está vacío, es el bundle OKF de contexto. Las tools okf_* operan
// dentro de él (rutas bundle-relativas, con leading / al estilo OKF).
var okfDir = ""

// okfTools se agregan a `tools` solo cuando hay un bundle (--okf). Gestión de
// contexto por divulgación progresiva: el índice va en el system prompt, los
// conceptos se leen bajo demanda, y el agente puede curar el bundle.
var okfTools = []Tool{
	{Type: "function", Function: ToolFunction{
		Name:        "okf_read",
		Description: "Lee un concepto del bundle OKF de contexto (frontmatter + cuerpo). path es bundle-relativo (ej: /metrica-ventas.md).",
		Parameters:  strSchema([]string{"path"}, prop{"path", "Ruta bundle-relativa del concepto"}),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "okf_search",
		Description: "Busca un regex dentro del CUERPO de los conceptos del bundle (tipo grep). Para saber qué conceptos existen o su title/type/description, usá el índice del system prompt — NO esto.",
		Parameters:  strSchema([]string{"pattern"}, prop{"pattern", "Patrón regex a buscar en el bundle"}),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "okf_write",
		Description: "Crea o actualiza un concepto OKF en el bundle: escribe frontmatter (type obligatorio) + cuerpo markdown. El usuario confirma antes.",
		Parameters: strSchema([]string{"path", "type", "body"},
			prop{"path", "Ruta bundle-relativa (ej: /clientes/acme.md)"},
			prop{"type", "type OKF, obligatorio (ej: Métrica, Playbook, API Endpoint)"},
			prop{"title", "Título legible"},
			prop{"description", "Resumen de una frase"},
			prop{"tags", "Tags separados por coma"},
			prop{"body", "Cuerpo markdown del concepto"}),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "okf_log",
		Description: "Anota una entrada fechada en el log.md del bundle (historial de cambios OKF). El usuario confirma antes.",
		Parameters:  strSchema([]string{"entry"}, prop{"entry", "La línea a registrar en el historial"}),
	}},
}

const maxToolOutput = 8000

func clip(s string) string {
	if len(s) > maxToolOutput {
		return s[:maxToolOutput] + "\n…(salida truncada)"
	}
	return s
}

// execTool corre una tool call en local y devuelve el resultado como texto.
func execTool(tc ToolCall, autoYes bool) string {
	switch tc.Name {
	case "list_dir":
		path, _ := tc.Arguments["path"].(string)
		if path == "" {
			path = "."
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		var b strings.Builder
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			b.WriteString(name + "\n")
		}
		if b.Len() == 0 {
			return "(directorio vacío)"
		}
		return clip(b.String())

	case "read_file":
		path, _ := tc.Arguments["path"].(string)
		if path == "" {
			return "ERROR: falta 'path'"
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		return clip(string(data))

	case "glob":
		pattern, _ := tc.Arguments["pattern"].(string)
		if pattern == "" {
			return "ERROR: falta 'pattern'"
		}
		matches, err := doGlob(pattern)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		if len(matches) == 0 {
			return "(sin coincidencias)"
		}
		const maxMatches = 200
		note := ""
		if len(matches) > maxMatches {
			note = fmt.Sprintf("\n…(%d más)", len(matches)-maxMatches)
			matches = matches[:maxMatches]
		}
		return strings.Join(matches, "\n") + note

	case "search":
		pattern, _ := tc.Arguments["pattern"].(string)
		root, _ := tc.Arguments["path"].(string)
		if pattern == "" {
			return "ERROR: falta 'pattern'"
		}
		if root == "" {
			root = "."
		}
		return doSearch(pattern, root)

	case "http_get":
		u, _ := tc.Arguments["url"].(string)
		if u == "" {
			return "ERROR: falta 'url'"
		}
		return httpGet(u)

	case "write_file":
		p, _ := tc.Arguments["path"].(string)
		content, _ := tc.Arguments["content"].(string)
		if p == "" {
			return "ERROR: falta 'path'"
		}
		if !autoYes && !confirm(fmt.Sprintf("\n  ⚠  el modelo quiere ESCRIBIR %d bytes en:\n      %s\n  ¿permitir? [y/N] ", len(content), p)) {
			return "El usuario DENEGÓ la escritura de este archivo."
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return "ERROR: " + err.Error()
		}
		return fmt.Sprintf("OK: escrito %s (%d bytes)", p, len(content))

	case "edit_file":
		p, _ := tc.Arguments["path"].(string)
		oldS, _ := tc.Arguments["old"].(string)
		newS, _ := tc.Arguments["new"].(string)
		if p == "" {
			return "ERROR: falta 'path'"
		}
		if oldS == "" {
			return "ERROR: falta 'old' (el texto exacto a reemplazar)"
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		content := string(data)
		switch strings.Count(content, oldS) {
		case 0:
			return "ERROR: 'old' no aparece en el archivo (debe coincidir literal, incluidos espacios y saltos de línea)."
		case 1:
			// ok
		default:
			return fmt.Sprintf("ERROR: 'old' aparece %d veces; debe ser único. Agregá contexto alrededor para desambiguar.", strings.Count(content, oldS))
		}
		if !autoYes && !confirm(fmt.Sprintf("\n  ⚠  el modelo quiere REEMPLAZAR en %s:\n      - %s\n      + %s\n  ¿permitir? [y/N] ", p, oneLine(oldS, 90), oneLine(newS, 90))) {
			return "El usuario DENEGÓ la edición de este archivo."
		}
		updated := strings.Replace(content, oldS, newS, 1)
		if err := os.WriteFile(p, []byte(updated), 0o644); err != nil {
			return "ERROR: " + err.Error()
		}
		return fmt.Sprintf("OK: reemplazado 1 fragmento en %s (%d → %d bytes)", p, len(content), len(updated))

	case "run_command":
		command, _ := tc.Arguments["command"].(string)
		if command == "" {
			return "ERROR: falta 'command'"
		}
		if !autoYes && !confirm(fmt.Sprintf("\n  ⚠  el modelo quiere ejecutar:\n      %s\n  ¿permitir? [y/N] ", command)) {
			return "El usuario DENEGÓ la ejecución de este comando."
		}
		return clip(runShell(command))

	case "okf_read":
		if okfDir == "" {
			return "ERROR: no hay bundle OKF (arrancá con --okf <dir>)"
		}
		p, _ := tc.Arguments["path"].(string)
		full, err := okfResolve(p)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		return clip(string(data))

	case "okf_search":
		if okfDir == "" {
			return "ERROR: no hay bundle OKF (arrancá con --okf <dir>)"
		}
		pattern, _ := tc.Arguments["pattern"].(string)
		if pattern == "" {
			return "ERROR: falta 'pattern'"
		}
		return doSearch(pattern, okfDir)

	case "okf_write":
		if okfDir == "" {
			return "ERROR: no hay bundle OKF (arrancá con --okf <dir>)"
		}
		p, _ := tc.Arguments["path"].(string)
		typ, _ := tc.Arguments["type"].(string)
		if p == "" || typ == "" {
			return "ERROR: 'path' y 'type' son obligatorios (type es el campo requerido de OKF)"
		}
		full, err := okfResolve(p)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		doc := buildOKFDoc(tc.Arguments)
		if !autoYes && !confirm(fmt.Sprintf("\n  ⚠  el modelo quiere ESCRIBIR el concepto OKF %s (type: %s, %d bytes):\n      %s\n  ¿permitir? [y/N] ", p, typ, len(doc), oneLine(doc, 90))) {
			return "El usuario DENEGÓ la escritura de este concepto."
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "ERROR: " + err.Error()
		}
		if err := os.WriteFile(full, []byte(doc), 0o644); err != nil {
			return "ERROR: " + err.Error()
		}
		return fmt.Sprintf("OK: concepto escrito en %s (%d bytes)", p, len(doc))

	case "okf_log":
		if okfDir == "" {
			return "ERROR: no hay bundle OKF (arrancá con --okf <dir>)"
		}
		entry, _ := tc.Arguments["entry"].(string)
		if entry == "" {
			return "ERROR: falta 'entry'"
		}
		if !autoYes && !confirm(fmt.Sprintf("\n  ⚠  el modelo quiere anotar en log.md:\n      %s\n  ¿permitir? [y/N] ", oneLine(entry, 90))) {
			return "El usuario DENEGÓ la anotación en el log."
		}
		return okfAppendLog(entry)

	default:
		return "ERROR: herramienta desconocida: " + tc.Name
	}
}

// confirm muestra el mensaje y devuelve true si el usuario tipeó "y".
func confirm(msg string) bool {
	fmt.Print(msg)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(strings.ToLower(line)) == "y"
}

// doGlob busca archivos por patrón. Soporta ** (recursivo) además de lo que
// entiende path.Match (* ? [..]). filepath.Glob no hace **, así que para esos
// patrones caminamos el árbol y matcheamos el sufijo de segmentos.
func doGlob(pattern string) ([]string, error) {
	pattern = filepath.ToSlash(pattern)
	if i := strings.Index(pattern, "**/"); i >= 0 {
		base := strings.TrimSuffix(pattern[:i], "/")
		if base == "" {
			base = "."
		}
		rest := pattern[i+3:]
		restN := strings.Count(rest, "/") + 1
		var matches []string
		err := filepath.WalkDir(base, func(pth string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			segs := strings.Split(filepath.ToSlash(pth), "/")
			if len(segs) >= restN {
				tail := strings.Join(segs[len(segs)-restN:], "/")
				if ok, _ := path.Match(rest, tail); ok {
					matches = append(matches, filepath.ToSlash(pth))
				}
			}
			return nil
		})
		return matches, err
	}
	m, err := filepath.Glob(filepath.FromSlash(pattern))
	for i := range m {
		m[i] = filepath.ToSlash(m[i])
	}
	return m, err
}

// httpGet hace un GET y devuelve status + content-type + cuerpo (truncado).
func httpGet(u string) string {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxToolOutput+1))
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return fmt.Sprintf("HTTP %d · %s\n%s", resp.StatusCode, resp.Header.Get("Content-Type"), clip(string(body)))
}

// doSearch busca un regex en archivos de texto bajo root, recursivo, tipo grep.
// Salta .git y node_modules, archivos binarios y los de más de 1 MB.
func doSearch(pattern, root string) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "ERROR: patrón inválido: " + err.Error()
	}
	const maxHits = 100
	var b strings.Builder
	hits := 0
	truncated := false

	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if hits >= maxHits {
			truncated = true
			return filepath.SkipAll
		}
		if info, e := d.Info(); e == nil && info.Size() > 1<<20 {
			return nil // > 1 MB: se salta
		}
		data, e := os.ReadFile(p)
		if e != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil // ilegible o binario
		}
		lineNo := 0
		for _, line := range strings.Split(string(data), "\n") {
			lineNo++
			if re.MatchString(line) {
				line = strings.TrimRight(line, "\r")
				if len(line) > 200 {
					line = line[:200] + "…"
				}
				fmt.Fprintf(&b, "%s:%d: %s\n", filepath.ToSlash(p), lineNo, line)
				hits++
				if hits >= maxHits {
					truncated = true
					break
				}
			}
		}
		return nil
	})

	if hits == 0 {
		return "(sin coincidencias)"
	}
	res := b.String()
	if truncated {
		res += fmt.Sprintf("…(cortado en %d coincidencias)\n", maxHits)
	}
	return clip(res)
}

// ── OKF: bundle de conocimiento como contexto ────────────────────────────────

// okfResolve mapea una ruta bundle-relativa a una ruta real, confinada al bundle
// (no deja escapar con ../).
func okfResolve(rel string) (string, error) {
	rel = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(rel)), "/")
	if rel == "" {
		return "", fmt.Errorf("path vacío")
	}
	full := filepath.Join(okfDir, filepath.FromSlash(rel))
	absDir, err1 := filepath.Abs(okfDir)
	absFull, err2 := filepath.Abs(full)
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("ruta inválida")
	}
	if absFull != absDir && !strings.HasPrefix(absFull, absDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("la ruta se sale del bundle OKF")
	}
	return full, nil
}

type okfMeta struct{ typ, title, desc string }

// parseFrontmatter lee el YAML del tope de un concepto OKF (parser mínimo: flat
// key: value). Conforma si `type` no está vacío.
func parseFrontmatter(content string) (okfMeta, bool) {
	if !strings.HasPrefix(content, "---") {
		return okfMeta{}, false
	}
	rest := content[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return okfMeta{}, false
	}
	var m okfMeta
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, ":")
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
		switch key {
		case "type":
			m.typ = val
		case "title":
			m.title = val
		case "description":
			m.desc = val
		}
	}
	return m, m.typ != ""
}

// buildOKFIndex arma el listado de conceptos (type · title — description) para
// la divulgación progresiva. Salta los reservados index.md y log.md.
func buildOKFIndex() string {
	var b strings.Builder
	count := 0
	filepath.WalkDir(okfDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		low := strings.ToLower(d.Name())
		if !strings.HasSuffix(low, ".md") || low == "index.md" || low == "log.md" {
			return nil
		}
		data, e := os.ReadFile(p)
		if e != nil {
			return nil
		}
		rel, _ := filepath.Rel(okfDir, p)
		rel = "/" + filepath.ToSlash(rel)
		m, ok := parseFrontmatter(string(data))
		if ok {
			line := "- " + rel + " — type: " + m.typ
			if m.title != "" {
				line += " · " + m.title
			}
			if m.desc != "" {
				line += " — " + m.desc
			}
			b.WriteString(line + "\n")
		} else {
			b.WriteString("- " + rel + " (sin frontmatter OKF)\n")
		}
		count++
		return nil
	})
	if count == 0 {
		return "(el bundle no tiene conceptos todavía)"
	}
	return b.String()
}

// okfSystemAddon es lo que se suma al system prompt: el índice + cómo usar las tools.
func okfSystemAddon() string {
	return "\n\nTenés un bundle de conocimiento OKF montado como contexto. El índice de abajo es COMPLETO: lista TODOS los conceptos con su type, título y descripción. Para preguntas sobre qué conceptos hay, o su título/type/descripción, respondé directo desde este índice — NO llames a okf_search ni okf_read para eso (ya tenés la info acá).\n\nÍndice de conceptos:\n" +
		buildOKFIndex() +
		"\nUsá okf_read(path) SOLO cuando necesites el CUERPO o el detalle de un concepto puntual. Usá okf_search(pattern) SOLO para encontrar un texto DENTRO de los cuerpos. Curá el conocimiento con okf_write (crear/actualizar; type obligatorio) y okf_log (anotar un cambio); guardá lo reutilizable como concepto."
}

// buildOKFDoc arma un concepto OKF válido (frontmatter con type obligatorio +
// timestamp ISO 8601 + cuerpo) a partir de los argumentos de la tool.
func buildOKFDoc(args map[string]any) string {
	get := func(k string) string { s, _ := args[k].(string); return strings.TrimSpace(s) }
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("type: " + get("type") + "\n")
	if v := get("title"); v != "" {
		b.WriteString("title: " + v + "\n")
	}
	if v := get("description"); v != "" {
		b.WriteString("description: " + v + "\n")
	}
	if v := get("tags"); v != "" {
		var ts []string
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				ts = append(ts, t)
			}
		}
		if len(ts) > 0 {
			b.WriteString("tags: [" + strings.Join(ts, ", ") + "]\n")
		}
	}
	b.WriteString("timestamp: " + time.Now().UTC().Format(time.RFC3339) + "\n")
	b.WriteString("---\n\n")
	body, _ := args["body"].(string)
	b.WriteString(strings.TrimRight(body, "\n") + "\n")
	return b.String()
}

// okfAppendLog agrega una entrada fechada al log.md del bundle.
func okfAppendLog(entry string) string {
	full, err := okfResolve("log.md")
	if err != nil {
		return "ERROR: " + err.Error()
	}
	line := "- " + time.Now().UTC().Format("2006-01-02") + ": " + strings.TrimSpace(entry) + "\n"
	f, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return "ERROR: " + err.Error()
	}
	return "OK: anotado en log.md — " + strings.TrimRight(line, "\n")
}

func runShell(command string) string {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	res := out.String()
	if err != nil {
		res += "\n[exit: " + err.Error() + "]"
	}
	if res == "" {
		res = "(sin salida)"
	}
	return res
}

// ── loop del agente ──────────────────────────────────────────────────────────

const systemPrompt = `Sos un agente que corre en la máquina del usuario. Disponés de estas herramientas, que se ejecutan en su equipo:
- list_dir(path): lista un directorio.
- read_file(path): lee un archivo de texto.
- glob(pattern): busca archivos por patrón (soporta ** recursivo).
- search(pattern, path): busca un regex DENTRO de archivos (tipo grep), recursivo.
- http_get(url): hace un GET HTTP y devuelve el cuerpo.
- write_file(path, content): escribe/sobrescribe un archivo (el usuario lo confirma).
- edit_file(path, old, new): reemplaza un fragmento exacto y único de un archivo (el usuario lo confirma).
- run_command(command): ejecuta un comando de shell (el usuario lo confirma).
Usalas cuando necesites información real del sistema, buscar archivos, traer algo de la web o escribir/correr algo. No inventes contenidos, resultados ni respuestas de red: pedilos con las herramientas. Cuando tengas la respuesta, contestá en español, breve y claro, SIN más tool calls.`

func generate(client *http.Client, url, secret string, req GenRequest) (*GenResponse, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest("POST", url+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+secret)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out GenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("respuesta no-JSON (HTTP %d): %w", resp.StatusCode, err)
	}
	return &out, nil
}

// generateStream pide con stream:true y consume el SSE, llamando onDelta con
// cada fragmento de texto a medida que llega. Devuelve el resultado final
// (texto completo + tool_calls + assistant) al recibir el evento done.
func generateStream(client *http.Client, url, secret string, req GenRequest, onDelta func(string)) (*GenResponse, error) {
	req.Stream = true
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest("POST", url+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+secret)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e GenResponse
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var full strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		var ev struct {
			Delta     string     `json:"delta"`
			Done      bool       `json:"done"`
			Think     string     `json:"think"`
			ToolCalls []ToolCall `json:"tool_calls"`
			Assistant string     `json:"assistant"`
			Error     string     `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		if ev.Error != "" {
			return nil, fmt.Errorf("%s", ev.Error)
		}
		if ev.Delta != "" {
			full.WriteString(ev.Delta)
			onDelta(ev.Delta)
		}
		if ev.Done {
			return &GenResponse{
				Text:      full.String(),
				Think:     ev.Think,
				ToolCalls: ev.ToolCalls,
				Assistant: ev.Assistant,
			}, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return &GenResponse{Text: full.String()}, nil // el stream cerró sin 'done'
}

// runTurn corre el loop de tool-calling para el estado actual de `messages` (que
// ya incluye el mensaje del usuario). Imprime la respuesta final —en streaming a
// medida que llega si stream=true, o de una si no— y devuelve el historial
// actualizado con el turno assistant agregado, para mantener el hilo.
func runTurn(client *http.Client, url, secret string, messages []Message, autoYes, stream bool) ([]Message, error) {
	const maxTurns = 8
	for turn := 1; turn <= maxTurns; turn++ {
		var resp *GenResponse
		var err error
		printed := false // ¿se imprimió texto ya (streaming)?

		if stream {
			resp, err = generateStream(client, url, secret, GenRequest{Messages: messages, Tools: tools}, func(d string) {
				if !printed {
					printed = true
					fmt.Print("\x1b[1m")
				}
				fmt.Print(d)
			})
			if printed {
				fmt.Print("\x1b[0m")
			}
		} else {
			fmt.Print("\x1b[90m· pensando…\x1b[0m\n")
			resp, err = generate(client, url, secret, GenRequest{Messages: messages, Tools: tools})
		}
		if err != nil {
			return messages, err
		}
		if resp.Error != "" {
			return messages, fmt.Errorf("API: %s", resp.Error)
		}
		if resp.Warning != "" {
			fmt.Println("\n  ⚠ " + resp.Warning)
		}

		// El turno assistant va VERBATIM (campo `assistant` de la API): tanto para
		// continuar un round-trip de tools como para dejar la respuesta final en el
		// historial y mantener el contexto entre turnos.
		messages = append(messages, Message{Role: "assistant", Content: resp.Assistant})

		if len(resp.ToolCalls) == 0 {
			if stream {
				if printed {
					fmt.Println()
				} else {
					fmt.Println("\x1b[90m(sin texto)\x1b[0m")
				}
			} else {
				fmt.Println("\x1b[1m" + strings.TrimSpace(resp.Text) + "\x1b[0m")
			}
			return messages, nil
		}
		if stream && printed {
			fmt.Println() // cerrar la línea del texto parcial antes de las tools
		}
		for _, tc := range resp.ToolCalls {
			args, _ := json.Marshal(tc.Arguments)
			fmt.Printf("\x1b[36m  → %s(%s)\x1b[0m\n", tc.Name, string(args))
			result := execTool(tc, autoYes)
			fmt.Printf("\x1b[90m    %s\x1b[0m\n", oneLine(result, 140))
			messages = append(messages, Message{Role: "tool", Content: result})
		}
	}
	return messages, fmt.Errorf("se alcanzó el máximo de turnos sin respuesta final")
}

func main() {
	url := envOr("BONSAI_URL", "https://bonsai-agent-lab.pages.dev")
	secret := os.Getenv("BONSAI_SECRET")
	if secret == "" {
		fatal("falta BONSAI_SECRET (el API_SECRET del deploy). Ej: set BONSAI_SECRET=tu-secreto")
	}

	autoYes, chat, stream := false, false, false
	var parts []string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--yes" || a == "-y":
			autoYes = true
		case a == "--chat" || a == "-c":
			chat = true
		case a == "--stream" || a == "-s":
			stream = true
		case a == "--okf":
			if i+1 < len(args) {
				i++
				okfDir = args[i]
			}
		case strings.HasPrefix(a, "--okf="):
			okfDir = a[len("--okf="):]
		default:
			parts = append(parts, a)
		}
	}
	prompt := strings.TrimSpace(strings.Join(parts, " "))

	// Bundle OKF de contexto: registra las tools okf_* y suma el índice (no el
	// contenido) al system prompt — divulgación progresiva.
	sys := systemPrompt
	if okfDir != "" {
		if info, err := os.Stat(okfDir); err != nil || !info.IsDir() {
			fatal("--okf: no es un directorio: " + okfDir)
		}
		tools = append(tools, okfTools...)
		sys += okfSystemAddon()
	}
	sysMsg := Message{Role: "system", Content: sys}

	client := &http.Client{Timeout: 6 * time.Minute}
	messages := []Message{sysMsg}

	// One-shot: hay prompt y NO se pidió chat.
	if prompt != "" && !chat {
		messages = append(messages, Message{Role: "user", Content: prompt})
		if _, err := runTurn(client, url, secret, messages, autoYes, stream); err != nil {
			fatal(err.Error())
		}
		return
	}

	// Chat interactivo: mantiene la conversación (el mismo `messages`) entre turnos.
	fmt.Println("\x1b[1mbonsai-agent\x1b[0m · chat — \x1b[90m/exit para salir · /reset borra el historial · /help\x1b[0m")
	reader := bufio.NewReader(os.Stdin)
	pending := prompt // si vino con -c "…", ese es el primer turno
	for {
		var line string
		if pending != "" {
			line, pending = pending, ""
			fmt.Println("\x1b[32myo>\x1b[0m " + line)
		} else {
			fmt.Print("\x1b[32myo>\x1b[0m ")
			l, err := reader.ReadString('\n')
			if err != nil { // EOF (Ctrl+Z en Windows, Ctrl+D en Unix)
				fmt.Println()
				return
			}
			line = strings.TrimSpace(l)
		}
		if line == "" {
			continue
		}
		switch line {
		case "/exit", "/quit":
			return
		case "/reset":
			messages = []Message{sysMsg}
			fmt.Println("\x1b[90m(historial reiniciado)\x1b[0m")
			continue
		case "/help":
			fmt.Println("\x1b[90mcomandos: /exit · /reset · /help. Tools: list_dir, read_file, run_command ([y/N]).\x1b[0m")
			continue
		}

		messages = append(messages, Message{Role: "user", Content: line})
		updated, err := runTurn(client, url, secret, messages, autoYes, stream)
		if err != nil {
			fmt.Println("\x1b[31merror:\x1b[0m " + err.Error())
			// deshacer el user message fallido para no dejar el hilo inconsistente
			messages = messages[:len(messages)-1]
			continue
		}
		messages = updated
		fmt.Println()
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "\x1b[31merror:\x1b[0m "+msg)
	os.Exit(1)
}
