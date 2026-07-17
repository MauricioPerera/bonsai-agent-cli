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
//	bonsai-agent "¿qué archivos hay acá y de qué trata el proyecto?"
//	bonsai-agent --yes "corré los tests"   (--yes: no pide confirmación de shell)
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
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

// param arma el JSON-Schema de un objeto con una sola propiedad string.
func strParam(name, desc string, required bool) map[string]any {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			name: map[string]any{"type": "string", "description": desc},
		},
	}
	if required {
		schema["required"] = []string{name}
	}
	return schema
}

var tools = []Tool{
	{Type: "function", Function: ToolFunction{
		Name:        "list_dir",
		Description: "Lista archivos y carpetas de un directorio. Si no se pasa path, usa el actual.",
		Parameters:  strParam("path", "Ruta del directorio", false),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "read_file",
		Description: "Lee y devuelve el contenido de un archivo de texto.",
		Parameters:  strParam("path", "Ruta del archivo a leer", true),
	}},
	{Type: "function", Function: ToolFunction{
		Name:        "run_command",
		Description: "Ejecuta un comando de shell en la máquina del usuario y devuelve su salida (stdout+stderr). El usuario confirma antes de correr.",
		Parameters:  strParam("command", "El comando a ejecutar", true),
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

	case "run_command":
		command, _ := tc.Arguments["command"].(string)
		if command == "" {
			return "ERROR: falta 'command'"
		}
		if !autoYes {
			fmt.Printf("\n  ⚠  el modelo quiere ejecutar:\n      %s\n  ¿permitir? [y/N] ", command)
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(strings.ToLower(line)) != "y" {
				return "El usuario DENEGÓ la ejecución de este comando."
			}
		}
		return clip(runShell(command))

	default:
		return "ERROR: herramienta desconocida: " + tc.Name
	}
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
- run_command(command): ejecuta un comando de shell (el usuario lo confirma).
Usalas cuando necesites información real del sistema o correr algo. No inventes el contenido de archivos ni la salida de comandos: pedilos con las herramientas. Cuando tengas la respuesta, contestá en español, breve y claro, SIN más tool calls.`

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

func main() {
	url := envOr("BONSAI_URL", "https://bonsai-agent-lab.pages.dev")
	secret := os.Getenv("BONSAI_SECRET")
	if secret == "" {
		fatal("falta BONSAI_SECRET (el API_SECRET del deploy). Ej: set BONSAI_SECRET=tu-secreto")
	}

	autoYes := false
	var parts []string
	for _, a := range os.Args[1:] {
		switch a {
		case "--yes", "-y":
			autoYes = true
		default:
			parts = append(parts, a)
		}
	}
	prompt := strings.TrimSpace(strings.Join(parts, " "))
	if prompt == "" {
		fmt.Print("prompt> ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		prompt = strings.TrimSpace(line)
	}
	if prompt == "" {
		fatal("prompt vacío")
	}

	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: prompt},
	}

	client := &http.Client{Timeout: 6 * time.Minute}
	const maxTurns = 8

	for turn := 1; turn <= maxTurns; turn++ {
		fmt.Printf("\x1b[90m· pensando (turno %d)…\x1b[0m\n", turn)
		resp, err := generate(client, url, secret, GenRequest{Messages: messages, Tools: tools})
		if err != nil {
			fatal(err.Error())
		}
		if resp.Error != "" {
			fatal("API: " + resp.Error)
		}
		if resp.Warning != "" {
			fmt.Println("  ⚠ " + resp.Warning)
		}

		if len(resp.ToolCalls) == 0 {
			fmt.Println("\n\x1b[1m" + strings.TrimSpace(resp.Text) + "\x1b[0m")
			return
		}

		// Continuar el round-trip: el turno assistant va VERBATIM (el campo
		// `assistant` que devuelve la API) y después un {role:"tool"} por llamada.
		messages = append(messages, Message{Role: "assistant", Content: resp.Assistant})
		for _, tc := range resp.ToolCalls {
			args, _ := json.Marshal(tc.Arguments)
			fmt.Printf("\x1b[36m  → %s(%s)\x1b[0m\n", tc.Name, string(args))
			result := execTool(tc, autoYes)
			fmt.Printf("\x1b[90m    %s\x1b[0m\n", oneLine(result, 140))
			messages = append(messages, Message{Role: "tool", Content: result})
		}
	}
	fmt.Println("\n(se alcanzó el máximo de turnos sin respuesta final)")
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
