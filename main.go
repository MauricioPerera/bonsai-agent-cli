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
//	bonsai-agent --yes "corré los tests"   (--yes: no pide confirmación de shell)
//
// Modos: con prompt y sin --chat es one-shot; sin prompt o con --chat/-c entra
// al chat (comandos /exit, /reset, /help).
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
		Name:        "run_command",
		Description: "Ejecuta un comando de shell en la máquina del usuario y devuelve su salida (stdout+stderr). El usuario confirma antes de correr.",
		Parameters:  strSchema([]string{"command"}, prop{"command", "El comando a ejecutar"}),
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

	case "run_command":
		command, _ := tc.Arguments["command"].(string)
		if command == "" {
			return "ERROR: falta 'command'"
		}
		if !autoYes && !confirm(fmt.Sprintf("\n  ⚠  el modelo quiere ejecutar:\n      %s\n  ¿permitir? [y/N] ", command)) {
			return "El usuario DENEGÓ la ejecución de este comando."
		}
		return clip(runShell(command))

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
- http_get(url): hace un GET HTTP y devuelve el cuerpo.
- write_file(path, content): escribe un archivo (el usuario lo confirma).
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

// runTurn corre el loop de tool-calling para el estado actual de `messages`
// (que ya incluye el nuevo mensaje del usuario) y devuelve el historial
// actualizado —con el turno assistant final agregado, para mantener el hilo— y
// el texto de la respuesta final.
func runTurn(client *http.Client, url, secret string, messages []Message, autoYes bool) ([]Message, string, error) {
	const maxTurns = 8
	for turn := 1; turn <= maxTurns; turn++ {
		fmt.Printf("\x1b[90m· pensando…\x1b[0m\n")
		resp, err := generate(client, url, secret, GenRequest{Messages: messages, Tools: tools})
		if err != nil {
			return messages, "", err
		}
		if resp.Error != "" {
			return messages, "", fmt.Errorf("API: %s", resp.Error)
		}
		if resp.Warning != "" {
			fmt.Println("  ⚠ " + resp.Warning)
		}

		// El turno assistant va VERBATIM (campo `assistant` de la API), siempre:
		// tanto para continuar un round-trip de tools como para dejar la respuesta
		// final en el historial y mantener el contexto entre turnos.
		messages = append(messages, Message{Role: "assistant", Content: resp.Assistant})

		if len(resp.ToolCalls) == 0 {
			return messages, strings.TrimSpace(resp.Text), nil
		}
		for _, tc := range resp.ToolCalls {
			args, _ := json.Marshal(tc.Arguments)
			fmt.Printf("\x1b[36m  → %s(%s)\x1b[0m\n", tc.Name, string(args))
			result := execTool(tc, autoYes)
			fmt.Printf("\x1b[90m    %s\x1b[0m\n", oneLine(result, 140))
			messages = append(messages, Message{Role: "tool", Content: result})
		}
	}
	return messages, "", fmt.Errorf("se alcanzó el máximo de turnos sin respuesta final")
}

func main() {
	url := envOr("BONSAI_URL", "https://bonsai-agent-lab.pages.dev")
	secret := os.Getenv("BONSAI_SECRET")
	if secret == "" {
		fatal("falta BONSAI_SECRET (el API_SECRET del deploy). Ej: set BONSAI_SECRET=tu-secreto")
	}

	autoYes, chat := false, false
	var parts []string
	for _, a := range os.Args[1:] {
		switch a {
		case "--yes", "-y":
			autoYes = true
		case "--chat", "-c":
			chat = true
		default:
			parts = append(parts, a)
		}
	}
	prompt := strings.TrimSpace(strings.Join(parts, " "))

	client := &http.Client{Timeout: 6 * time.Minute}
	messages := []Message{{Role: "system", Content: systemPrompt}}

	// One-shot: hay prompt y NO se pidió chat.
	if prompt != "" && !chat {
		messages = append(messages, Message{Role: "user", Content: prompt})
		_, text, err := runTurn(client, url, secret, messages, autoYes)
		if err != nil {
			fatal(err.Error())
		}
		fmt.Println("\n\x1b[1m" + text + "\x1b[0m")
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
			messages = []Message{{Role: "system", Content: systemPrompt}}
			fmt.Println("\x1b[90m(historial reiniciado)\x1b[0m")
			continue
		case "/help":
			fmt.Println("\x1b[90mcomandos: /exit · /reset · /help. Tools: list_dir, read_file, run_command ([y/N]).\x1b[0m")
			continue
		}

		messages = append(messages, Message{Role: "user", Content: line})
		updated, text, err := runTurn(client, url, secret, messages, autoYes)
		if err != nil {
			fmt.Println("\x1b[31merror:\x1b[0m " + err.Error())
			// deshacer el user message fallido para no dejar el hilo inconsistente
			messages = messages[:len(messages)-1]
			continue
		}
		messages = updated
		fmt.Println("\x1b[1m" + text + "\x1b[0m\n")
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
