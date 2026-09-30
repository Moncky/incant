# incant

**Ask for a shell one-liner without leaving your prompt — and see what it will do before you run it.**

![incant demo](demo/demo.gif)

Type what you want at the prompt, press <kbd>Ctrl-X</kbd> <kbd>Ctrl-I</kbd>, and the line becomes a command. Underneath, incant tells you what that command would change **here**:

```
~/app ❯ find logs -name '*.log' -mtime +7 -delete
⚠ deletes 30 files (501B) — ./logs
```

Nothing runs until you press Enter.

## Why incant

- **It looks before it answers.** incant can peek at your files — through a read-only sandbox — so `sum the amount column` works on a semicolon-delimited CSV, and `count the 404s` finds the right field in your log format instead of guessing.
- **It knows your machine.** BSD or GNU `sed`, whether `jq` or `rg` exists, your git state. `sed -i` on macOS is not `sed -i` on Linux, and incant writes for the one you have.
- **It shows the blast radius.** Every suggestion — and any command you type — gets a preview of what it would delete, overwrite, edit, move or discard, computed without running anything. It catches the mistakes you can't see by reading:

  ```
  find . -name node_modules -delete   ⚠ deletes nothing — -delete skips 1 non-empty dir
  sort data.txt > data.txt            ⚠ ./data.txt is emptied by > before sort reads it
  sed -i 's/a/b/' *.conf   (macOS)    ⚠ BSD sed takes 's/a/b/' as the -i backup suffix, not the script
  for f in *.log; do rm "$f"; done    ⚠ can't preview rm "$f" (variable)
  ```

- **It never runs what it suggests.** That is enforced by the code, not promised: only one file in the codebase can start a process, and a test fails the build if that changes.

## Install

```sh
brew install callumscott/tap/incant
# or
go install github.com/callumscott/incant/cmd/incant@latest
```

Prebuilt binaries for macOS and Linux are on the [releases page](https://github.com/callumscott/incant/releases).

Then add one line to your shell's rc file and open a new shell:

```sh
eval "$(incant shell-init zsh)"     # ~/.zshrc
eval "$(incant shell-init bash)"    # ~/.bashrc — needs bash 4+ (macOS: brew install bash)
```

Finally, give it credentials — either works:

```sh
export ANTHROPIC_API_KEY=sk-ant-...     # fast path: direct API, can look at files
# or just have the `claude` CLI logged in — slower (seconds), can't look at files
```

Check everything with:

```sh
incant doctor          # config, credentials, shell hook, and exactly what gets sent
incant doctor --ping   # also time one real request
```

## Keys

| Keys | What it does |
| --- | --- |
| <kbd>Ctrl-X</kbd> <kbd>Ctrl-I</kbd> | Turn the current line into a command. Press again for the next alternate. |
| <kbd>Ctrl-X</kbd> <kbd>Ctrl-U</kbd> | Put back the request you typed. (Otherwise zsh's normal undo.) |
| <kbd>Ctrl-X</kbd> <kbd>Ctrl-F</kbd> | Repair the command that just failed. |
| <kbd>Ctrl-X</kbd> <kbd>Ctrl-P</kbd> | Preview what the current line would change — works on anything, typed or pasted. |

Or call it directly: `incant "total line count of the txt files here"` puts the command at your next prompt (zsh) or in history (bash — press ↑).

`incant --check 'rm -rf build'` previews a command without asking a model.

## Configuration

Optional. `~/.config/incant/config` (or `$XDG_CONFIG_HOME/incant/config`):

```ini
# auto: the API when credentials exist, else the claude CLI
backend = auto            # auto | api | claude
model = claude-opus-5-5   # any Claude model id
effort = low              # low | medium | high | xhigh | max
candidates = 3            # alternates to cycle through with repeated Ctrl-X Ctrl-I (1-5)
context = full            # full | minimal | none — see Privacy
preview = true
# api_key = sk-ant-...    # prefer ANTHROPIC_API_KEY; if set here, chmod 600 the file
```

**Speed.** incant runs at `effort = low` on `claude-opus-5-5` by default. For the snappiest keybinding, `model = claude-haiku-4-5` is faster and cheaper, at some cost in accuracy on harder requests; `incant doctor --ping` shows what you get.

## Privacy

With each request incant sends your request text plus a short session block. **`incant doctor` prints the exact block for the directory you're in.** At `context = full` (the default) it holds:

- your OS, shell, and which common tools are installed
- the current directory's path and a listing of it (names and sizes, two levels deep)
- git branch and changed file names
- the previous command and its exit status

With the API backend, the model may also call read-only probes: the first/last lines of a text file, a regex search, file metadata. Credential files (`.env`, `*.pem`, `~/.ssh`, …) are never read, and nothing outside the current directory is reachable.

To send less:

- `context = minimal` — no listing, git state or file probes.
- `context = none` — also no previous command.
- A **`.incantignore`** file hides paths from the model, gitignore-style, for its directory and everything below:

  ```
  customers/
  *.sql
  ```

  An **empty** `.incantignore` makes the whole tree private: no listing, no git state, no probes.

The effect preview runs entirely on your machine and ignores `.incantignore` — hiding a file from it would understate what a command deletes.

**Fixing with error output (zsh, opt-in).** By default <kbd>Ctrl-X</kbd> <kbd>Ctrl-F</kbd> sees only the failed command and its exit code. To let it read the error message too, set `INCANT_CAPTURE_STDERR=1` before the `eval` line. While each command runs, its stderr is then copied (via `tee`) to a private temp file, and sent when you ask for a fix. The trade-off: programs that check whether stderr is a terminal will drop colours and progress bars.

## How the preview works

incant lexes the command and evaluates the parts that change files — `rm`, `find -delete`/`-exec`, `find | xargs`, `mv`/`cp` clobbering, `sed -i`/`perl -i`, `>` redirects, `chmod -R`, `truncate`, `git reset --hard`/`checkout -- .`/`restore`/`clean -f`, and more — against your real filesystem, in Go, read-only. Globs (including zsh `**`), `find` expressions (with GNU/BSD `-mtime` rounding), `cd` and subshells are followed.

When it can't be sure — a `$VAR` operand, a path outside the current directory, `find -exec sh -c` — it says **can't preview** rather than staying quiet, because silence reads as "harmless".

It trusts command names: an alias or function named `rm` is not detected.

## Development

```sh
go test ./...                                  # everything, including the eval harness self-test
INCANT_EVAL=1 go test ./eval -run TestEval -v  # score real answers (calls the model; costs money)
```

The eval runs each case in [`eval/cases.json`](eval/cases.json) — a request, a fixture directory, an expected outcome — through the configured backend, executes the command in a throwaway directory, and grades what happened. Commands whose preview reaches outside the fixture are failed unrun. Add a case whenever incant gets something wrong.

Layout: `internal/backend` (API tool loop, claude CLI fallback), `internal/probe` (the sandbox), `internal/impact` (the preview), `internal/shellinit/scripts` (the shell integrations — the real product), `internal/spawn` (the only code that may start a process).

Releases: `git tag v0.1.0 && git push --tags` runs GoReleaser (`.goreleaser.yaml`), which publishes binaries and the Homebrew formula.
