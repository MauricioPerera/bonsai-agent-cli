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

bonsai-agent.exe "listá los archivos de esta carpeta y decime de qué trata el proyecto"
bonsai-agent.exe "¿qué versión de go tengo?"        # pedirá confirmación para el shell
bonsai-agent.exe --yes "corré go version"           # --yes: no pregunta antes del shell
```

Sin argumentos, pide el prompt por stdin.

Necesita una pestaña de `bonsai-agent-lab.pages.dev` abierta con el **modelo
cargado** y el secreto pegado (es el worker que genera). Verificá con
`GET /api/status` que diga `model_loaded: true`.

## Herramientas

| Tool | Qué hace | Confirmación |
|------|----------|--------------|
| `list_dir(path)` | lista un directorio (default: el actual) | no (solo lectura) |
| `read_file(path)` | lee un archivo de texto | no (solo lectura) |
| `run_command(command)` | ejecuta un comando de shell y devuelve su salida | **sí, `[y/N]`** (salvo `--yes`) |

`run_command` usa `cmd /c` en Windows y `sh -c` en el resto. La salida se
trunca a 8 KB por herramienta para no saturar el contexto.

## Cómo funciona el loop

1. Manda `{messages, tools}` a `POST /api/generate`.
2. Si la respuesta trae `tool_calls`, ejecuta cada una en local.
3. Continúa el round-trip agregando el turno `assistant` (que la API devuelve
   verbatim) + un `{role:"tool","content":"<resultado>"}` por llamada, y vuelve a 1.
4. Cuando el modelo responde sin `tool_calls`, imprime el texto final. (Máximo 8
   turnos por las dudas.)

## Seguridad

- `run_command` pide confirmación `[y/N]` mostrando el comando exacto antes de
  correrlo. `--yes` lo desactiva — usalo solo si sabés qué vas a pedir.
- El modelo no ejecuta nada: propone, vos (o `--yes`) autorizás, el binario corre.

## No incluido / atribución

- **Modelo/app:** [`webml-community/bonsai-webgpu-kernels`](https://huggingface.co/spaces/webml-community/bonsai-webgpu-kernels) ·
  [`prism-ml/Bonsai-27B-gguf`](https://huggingface.co/prism-ml/Bonsai-27B-gguf) — de sus autores.

MIT para el código de acá.
