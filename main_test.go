package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockServer devuelve un /api/generate falso: la respuesta N-ésima la decide `plan`.
func mockServer(t *testing.T, plan func(call int) GenResponse) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(plan(calls))
	}))
	return srv, &calls
}

// Cuando el usuario pide guardar y el modelo NO llama ninguna herramienta, el
// agente debe empujarlo una vez y reintentar; en el reintento se escribe.
func TestRetryNudgeOnMissedSave(t *testing.T) {
	okfDir = t.TempDir()
	tools = append([]Tool{}, okfTools...)

	srv, calls := mockServer(t, func(call int) GenResponse {
		switch call {
		case 1: // narra, sin tool calls
			return GenResponse{Text: "Sería un concepto sobre política de vacaciones.", Assistant: "Sería un concepto sobre política de vacaciones."}
		case 2: // tras el nudge: llama okf_write
			return GenResponse{ToolCalls: []ToolCall{{Name: "okf_write", Arguments: map[string]any{
				"path": "/vacaciones.md", "type": "Playbook", "body": "15 dias por anio.",
			}}}, Assistant: "<tool_call>…"}
		default: // respuesta final
			return GenResponse{Text: "Concepto guardado.", Assistant: "Concepto guardado."}
		}
	})
	defer srv.Close()

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "Guardá un concepto sobre la política de vacaciones."},
	}
	if _, err := runTurn(&http.Client{Timeout: 5 * time.Second}, srv.URL, "secret", msgs, true, false); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if *calls < 2 {
		t.Fatalf("esperaba un reintento (>=2 llamadas al modelo), hubo %d", *calls)
	}
	if _, err := os.Stat(filepath.Join(okfDir, "vacaciones.md")); err != nil {
		t.Fatalf("el concepto no se escribió tras el reintento: %v", err)
	}
}

// Si el modelo llama la herramienta en el primer intento, NO debe haber nudge
// extra: exactamente el turno de la tool + la respuesta final.
func TestNoNudgeWhenToolCalledFirst(t *testing.T) {
	okfDir = t.TempDir()
	tools = append([]Tool{}, okfTools...)

	srv, calls := mockServer(t, func(call int) GenResponse {
		if call == 1 {
			return GenResponse{ToolCalls: []ToolCall{{Name: "okf_write", Arguments: map[string]any{
				"path": "/x.md", "type": "Nota", "body": "hola",
			}}}, Assistant: "x"}
		}
		return GenResponse{Text: "listo", Assistant: "listo"}
	})
	defer srv.Close()

	msgs := []Message{{Role: "user", Content: "Guardá X como concepto."}}
	if _, err := runTurn(&http.Client{Timeout: 5 * time.Second}, srv.URL, "s", msgs, true, false); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("esperaba 2 llamadas (tool + final), hubo %d", *calls)
	}
}

// Sin bundle OKF, aunque el usuario diga "guardá", no hay nudge (no hay dónde).
func TestNoNudgeWithoutOKF(t *testing.T) {
	okfDir = ""
	tools = []Tool{}

	srv, calls := mockServer(t, func(call int) GenResponse {
		return GenResponse{Text: "ok", Assistant: "ok"}
	})
	defer srv.Close()

	msgs := []Message{{Role: "user", Content: "Guardá esto."}}
	if _, err := runTurn(&http.Client{Timeout: 5 * time.Second}, srv.URL, "s", msgs, true, false); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("sin OKF no debería reintentar: esperaba 1 llamada, hubo %d", *calls)
	}
}

// okf_write avisa (en el resultado) cuando pisa un concepto existente.
func TestOkfWriteOverwriteNotice(t *testing.T) {
	okfDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(okfDir, "x.md"), []byte("viejo"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := execTool(ToolCall{Name: "okf_write", Arguments: map[string]any{
		"path": "/x.md", "type": "Nota", "body": "nuevo",
	}}, true)
	if !strings.Contains(res, "SOBRESCRITO") {
		t.Fatalf("esperaba aviso de sobrescritura, got: %q", res)
	}
	res2 := execTool(ToolCall{Name: "okf_write", Arguments: map[string]any{
		"path": "/nuevo.md", "type": "Nota", "body": "hola",
	}}, true)
	if strings.Contains(res2, "SOBRESCRITO") {
		t.Fatalf("un archivo nuevo no debería avisar sobrescritura: %q", res2)
	}
}

func TestLastUserWantsSave(t *testing.T) {
	yes := []string{"guardá esto", "creá un concepto nuevo", "actualizá la métrica", "anotá en el log", "documentá el proceso"}
	no := []string{"¿qué conceptos hay?", "leé el playbook", "explicá qué es RAG"}
	for _, s := range yes {
		if !lastUserWantsSave([]Message{{Role: "user", Content: s}}) {
			t.Errorf("debería detectar intención de guardar: %q", s)
		}
	}
	for _, s := range no {
		if lastUserWantsSave([]Message{{Role: "user", Content: s}}) {
			t.Errorf("NO debería detectar intención de guardar: %q", s)
		}
	}
}
