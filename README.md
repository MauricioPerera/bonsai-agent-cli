# bonsai-agent

Un **agente CLI en Go** sobre la API de **Bonsai-27B**. El modelo de 27B corre
100% en una pestaña del navegador (WebGPU) y la API lo relaya; este binario le
pone adelante un **loop de tool-calling** y **ejecuta las herramientas en tu
máquina**. El modelo solo *pide* llamar herramientas — nunca ejecuta nada por su
cuenta: quien las corre es este programa, en tu equipo, y las que tocan el
sistema te piden confirmación.

Un solo `.exe`, sin runtime: nada de Python/Node ni políticas de ejecución de
PowerShell.

**Qué trae:**
- **8 herramientas** de sistema (leer/buscar/escribir/editar archivos, HTTP, shell) + **4 de OKF**.
- **Modos:** one-shot, **chat** interactivo con memoria, y **streaming** de la respuesta token a token.
- **Gestión de contexto OKF**: montás un bundle de conocimiento como base/memoria del agente.

## Compilar

```sh
go build -o bonsai-agent.exe .
```

## Configurar

```sh
set BONSAI_SECRET=tu-secreto     # el API_SECRET del deploy (obligatorio)
set BONSAI_URL=https://bonsai-agent-lab.pages.dev   # opcional (es el default)
```

Necesita una pestaña de `bonsai-agent-lab.pages.dev` abierta con el **modelo
cargado** y el secreto pegado en el panel: esa pestaña es el worker que genera.
Verificá con `GET /api/status` que diga `model_loaded: true`.

## Opciones

| Flag | Qué hace |
|------|----------|
| *(prompt como argumento)* | one-shot: una pregunta → una respuesta |
| `--chat`, `-c` | chat interactivo (mantiene la conversación entre turnos) |
| `--stream`, `-s` | imprime la respuesta a medida que se genera (SSE) |
| `--okf <dir>` | monta un bundle OKF como contexto/memoria |
| `--yes`, `-y` | no pide confirmación `[y/N]` para las herramientas que modifican el sistema |

Se combinan libremente (`-c -s --okf ./kb`). Sin prompt (o con `--chat`) entra al
chat; con prompt y sin `--chat` es one-shot. Comandos dentro del chat: `/exit`,
`/reset` (borra el historial), `/help`.

## Usar

```sh
# one-shot
bonsai-agent.exe "listá los archivos de esta carpeta y decime de qué trata el proyecto"
bonsai-agent.exe "¿qué versión de go tengo?"        # pedirá confirmación para el shell
bonsai-agent.exe --yes "corré go version"           # --yes: no pregunta antes del shell

# chat interactivo (recuerda la conversación, incluidos los resultados de tools)
bonsai-agent.exe --chat
bonsai-agent.exe -c "arrancá averiguando qué proyecto es este"

# streaming (token a token)
bonsai-agent.exe --stream "escribime un haiku sobre bonsáis"
bonsai-agent.exe -c -s                              # chat + streaming

# contexto OKF
bonsai-agent.exe --okf example-okf "¿cómo se calcula la métrica de ventas?"
bonsai-agent.exe --okf ./mi-kb -c -s                # chat + streaming + base OKF
```

## Herramientas

Siempre disponibles:

| Tool | Qué hace | Confirmación |
|------|----------|--------------|
| `list_dir(path)` | lista un directorio (default: el actual) | no (solo lectura) |
| `read_file(path)` | lee un archivo de texto | no (solo lectura) |
| `glob(pattern)` | busca archivos por patrón — `*`, `?`, `[..]` y `**` recursivo (ej: `**/*.go`) | no (solo lectura) |
| `search(pattern, path)` | busca un regex (RE2) *dentro* de archivos, recursivo — devuelve `archivo:línea: contenido` | no (solo lectura) |
| `http_get(url)` | hace un GET HTTP y devuelve status + content-type + cuerpo | no (solo lectura) |
| `write_file(path, content)` | escribe/sobrescribe un archivo de texto | **`[y/N]`** (salvo `--yes`) |
| `edit_file(path, old, new)` | reemplaza un fragmento exacto y **único** (`old` literal); muestra un diff | **`[y/N]`** (salvo `--yes`) |
| `run_command(command)` | ejecuta un comando de shell y devuelve su salida | **`[y/N]`** (salvo `--yes`) |

`run_command` usa `cmd /c` en Windows y `sh -c` en el resto. La salida se trunca
a 8 KB por herramienta (y `http_get` limita el cuerpo a lo mismo). `glob` devuelve
hasta 200 archivos; `search` hasta 100 líneas y salta `.git`, `node_modules`,
binarios y archivos de más de 1 MB.

## Gestión de contexto con OKF

Con `--okf <dir>` el agente monta un **bundle [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)**
(Open Knowledge Format) como base de conocimiento. Un bundle es un árbol de `.md`,
cada uno un *concepto* con frontmatter YAML (`type` obligatorio;
`title`/`description`/`tags`/`timestamp` recomendados) + cuerpo markdown. Los
archivos `index.md` y `log.md` son reservados.

Funciona por **divulgación progresiva**: al arrancar, el agente recibe en su
system prompt solo el **índice** (type · title — description de cada concepto), no
los cuerpos — así el contexto es barato. Las preguntas de nivel índice ("qué
conceptos hay", su tipo/título) se responden directo desde ahí, sin gastar una
tool call; el cuerpo de un concepto se lee **bajo demanda**. Y el agente puede
**curar** el bundle (memoria).

Tools que se activan solo con `--okf`:

| Tool | Qué hace | Confirmación |
|------|----------|--------------|
| `okf_read(path)` | lee un concepto (frontmatter + cuerpo); `path` bundle-relativo (`/ventas.md`) | no |
| `okf_search(pattern)` | grep dentro del *cuerpo* de los conceptos | no |
| `okf_write(path, type, title?, description?, tags?, body)` | crea/actualiza un concepto OKF válido (frontmatter + `timestamp` ISO 8601 automático) | **`[y/N]`** |
| `okf_log(entry)` | anota una entrada fechada en `log.md` | **`[y/N]`** |

Las rutas están confinadas al bundle (no se escapa con `../`). Hay un bundle de
ejemplo en [`example-okf/`](example-okf).

## Cómo funciona

1. Manda `{messages, tools}` a `POST /api/generate` (con `stream:true` si `-s`).
2. Si la respuesta trae `tool_calls`, ejecuta cada una en local.
3. Continúa el round-trip agregando el turno `assistant` (que la API devuelve
   verbatim) + un `{role:"tool","content":"<resultado>"}` por llamada, y vuelve a 1.
4. Cuando el modelo responde sin `tool_calls`, muestra el texto final —de una, o
   token a token si hay streaming. (Máximo 8 turnos por las dudas.)

En **streaming**, el worker del navegador publica el texto parcial mientras
genera y `/api/generate` lo relaya por **Server-Sent Events**; el agente lo
imprime a medida que llega. Sin `-s`, la API es síncrona (devuelve el texto
entero de una).

## Seguridad

- Las herramientas que **modifican tu sistema** — `run_command`, `write_file`,
  `edit_file`, `okf_write`, `okf_log` — piden confirmación `[y/N]` mostrando qué
  van a hacer (el comando, el path y bytes, el diff `- old / + new`) antes de
  tocar nada. `--yes` desactiva esas preguntas — usalo solo si sabés qué vas a
  pedir. Las de solo lectura corren sin preguntar.
- El modelo no ejecuta nada: propone, vos (o `--yes`) autorizás, el binario corre.

## No incluido / atribución

- **Modelo/app:** [`webml-community/bonsai-webgpu-kernels`](https://huggingface.co/spaces/webml-community/bonsai-webgpu-kernels) ·
  [`prism-ml/Bonsai-27B-gguf`](https://huggingface.co/prism-ml/Bonsai-27B-gguf) — de sus autores.
- **OKF:** [Open Knowledge Format spec](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md).

MIT para el código de acá.
