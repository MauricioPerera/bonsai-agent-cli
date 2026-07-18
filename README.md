# bonsai-agent

Un **agente CLI en Go** sobre la API de **Bonsai-27B**. El modelo de 27B corre
100% en una pestaña del navegador (WebGPU) y la API lo relaya; este binario le
pone adelante un **loop de tool-calling** y **ejecuta las herramientas en tu
máquina**. El modelo solo *pide* llamar herramientas — nunca ejecuta nada por su
cuenta: quien las corre es este programa, en tu equipo.

Un solo `.exe`, sin runtime: nada de Python/Node ni políticas de ejecución de
PowerShell.

## Compilar

```sh
go build -o bonsai-agent.exe .
```

## Usar

```sh
set BONSAI_SECRET=tu-secreto           # el API_SECRET del deploy
# opcional: set BONSAI_URL=https://bonsai-agent-lab.pages.dev   (es el default)

# one-shot (una pregunta, una respuesta)
bonsai-agent.exe "listá los archivos de esta carpeta y decime de qué trata el proyecto"
bonsai-agent.exe "¿qué versión de go tengo?"        # pedirá confirmación para el shell
bonsai-agent.exe --yes "corré go version"           # --yes: no pregunta antes del shell

# chat interactivo (mantiene la conversación)
bonsai-agent.exe --chat
bonsai-agent.exe -c "arrancá averiguando qué proyecto es este"   # con un primer turno

# streaming: imprime la respuesta a medida que se genera (SSE)
bonsai-agent.exe --stream "escribime un haiku sobre bonsáis"
bonsai-agent.exe -c -s                                           # chat + streaming
```

`--stream`/`-s` se combina con one-shot o `--chat`. La API es síncrona por
defecto; con `stream:true` el worker publica el texto parcial y `/api/generate`
lo relaya por **Server-Sent Events**.

```sh
# gestión de contexto con un bundle OKF
bonsai-agent.exe --okf example-okf "¿cómo se calcula la métrica de ventas?"
bonsai-agent.exe --okf ./mi-kb -c   # chat con la base de conocimiento montada
```

**Modos:** con prompt y sin `--chat` es one-shot; sin prompt (o con `--chat`/`-c`)
entra al chat interactivo, que **recuerda la conversación** entre turnos —incluidos
los resultados de las herramientas. Comandos dentro del chat: `/exit`, `/reset`
(borra el historial), `/help`.

Necesita una pestaña de `bonsai-agent-lab.pages.dev` abierta con el **modelo
cargado** y el secreto pegado (es el worker que genera). Verificá con
`GET /api/status` que diga `model_loaded: true`.

## Herramientas

| Tool | Qué hace | Confirmación |
|------|----------|--------------|
| `list_dir(path)` | lista un directorio (default: el actual) | no (solo lectura) |
| `read_file(path)` | lee un archivo de texto | no (solo lectura) |
| `glob(pattern)` | busca archivos por patrón — `*`, `?`, `[..]` y `**` recursivo (ej: `**/*.go`) | no (solo lectura) |
| `search(pattern, path)` | busca un regex (RE2) *dentro* de archivos, recursivo — devuelve `archivo:línea: contenido` | no (solo lectura) |
| `http_get(url)` | hace un GET HTTP y devuelve status + content-type + cuerpo | no (solo lectura) |
| `write_file(path, content)` | escribe/sobrescribe un archivo de texto | **sí, `[y/N]`** (salvo `--yes`) |
| `edit_file(path, old, new)` | reemplaza un fragmento exacto y **único** (`old` debe coincidir literal); muestra un diff | **sí, `[y/N]`** (salvo `--yes`) |
| `run_command(command)` | ejecuta un comando de shell y devuelve su salida | **sí, `[y/N]`** (salvo `--yes`) |

`run_command` usa `cmd /c` en Windows y `sh -c` en el resto. La salida se
trunca a 8 KB por herramienta (y `http_get` limita el cuerpo a lo mismo) para no
saturar el contexto. `glob` devuelve como máximo 200 archivos; `search` como
máximo 100 líneas y salta `.git`, `node_modules`, binarios y archivos de más de
1 MB.

## Cómo funciona el loop

1. Manda `{messages, tools}` a `POST /api/generate`.
2. Si la respuesta trae `tool_calls`, ejecuta cada una en local.
3. Continúa el round-trip agregando el turno `assistant` (que la API devuelve
   verbatim) + un `{role:"tool","content":"<resultado>"}` por llamada, y vuelve a 1.
4. Cuando el modelo responde sin `tool_calls`, imprime el texto final. (Máximo 8
   turnos por las dudas.)

## Gestión de contexto con OKF

Con `--okf <dir>` el agente monta un **bundle [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)** (Open Knowledge Format) como base de conocimiento. Un bundle es un árbol de `.md`, cada uno un *concepto* con frontmatter YAML (`type` obligatorio; `title`/`description`/`tags`/`timestamp` recomendados) + cuerpo markdown.

Funciona por **divulgación progresiva**: al arrancar, el agente recibe en su system prompt solo el **índice** (type · title — description de cada concepto), no el contenido — así el contexto es barato. Después lee el concepto que necesita bajo demanda. Y puede **curar** el bundle (memoria).

Tools que se activan solo con `--okf`:

| Tool | Qué hace | Confirmación |
|------|----------|--------------|
| `okf_read(path)` | lee un concepto (frontmatter + cuerpo); `path` es bundle-relativo (`/ventas.md`) | no |
| `okf_search(pattern)` | grep dentro del bundle | no |
| `okf_write(path, type, title?, description?, tags?, body)` | crea/actualiza un concepto OKF válido (frontmatter + `timestamp` ISO 8601 automático) | **`[y/N]`** |
| `okf_log(entry)` | anota una entrada fechada en `log.md` (historial de cambios) | **`[y/N]`** |

Las rutas están confinadas al bundle (no se puede escapar con `../`). Hay un bundle de ejemplo en [`example-okf/`](example-okf).

## Seguridad

- Las herramientas que **modifican tu sistema** — `run_command`, `write_file` y
  `edit_file` — piden confirmación `[y/N]` mostrando qué van a hacer (el comando
  exacto, el path y bytes, o el diff `- old / + new`) antes de tocar nada.
  `--yes` desactiva esas preguntas — usalo solo si sabés qué vas a pedir. Las de
  solo lectura (`list_dir`, `read_file`, `glob`, `search`, `http_get`) corren sin
  preguntar.
- El modelo no ejecuta nada: propone, vos (o `--yes`) autorizás, el binario corre.

## No incluido / atribución

- **Modelo/app:** [`webml-community/bonsai-webgpu-kernels`](https://huggingface.co/spaces/webml-community/bonsai-webgpu-kernels) ·
  [`prism-ml/Bonsai-27B-gguf`](https://huggingface.co/prism-ml/Bonsai-27B-gguf) — de sus autores.

MIT para el código de acá.
