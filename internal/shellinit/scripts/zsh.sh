# incant — zsh integration.
#
# Install with:   eval "$(incant shell-init zsh)"
#
# This file is the actual product. The binary only ever prints a command to
# stdout; everything that makes incant feel like part of the shell rather than
# a tool you call happens here.
#
# Keys:
#   ^X^I   turn the current line into a command; press again to cycle alternates
#   ^X^U   put back the request you typed (otherwise: zsh's usual undo)
#   ^X^F   repair the previous failed command
#   ^X^P   preview what the current line would change
#
# Set INCANT_CAPTURE_STDERR=1 before the eval line to let ^X^F see the failed
# command's error output. It is off by default because it pipes stderr through
# tee while each command runs: programs that check whether stderr is a
# terminal then drop colours and progress bars, and the error text is sent to
# the model when you ask for a fix.

# --------------------------------------------------------------------------
# Outcome hook
#
# Records each command and its exit status so `incant --fix` can diagnose a
# real failure instead of asking the user to describe one.
#
# The variables are deliberately NOT exported: they hold the user's command
# history, and every process they launch has no business inheriting it. The
# widget passes them explicitly instead.
# --------------------------------------------------------------------------

typeset -g _INCANT_PENDING=""
typeset -g _INCANT_LAST_CMD=""
typeset -g _INCANT_LAST_STATUS=0
typeset -g _INCANT_LAST_QUERY=""
typeset -g _INCANT_ERR=""
typeset -ga _INCANT_CANDIDATES=()
typeset -gi _INCANT_INDEX=0

_incant_preexec() {
  _INCANT_PENDING=$1
  (( $+functions[_incant_stderr_start] )) && _incant_stderr_start
}

_incant_precmd() {
  # $? must be read before anything else in this function.
  local st=$?
  (( $+functions[_incant_stderr_stop] )) && _incant_stderr_stop
  if [[ -n $_INCANT_PENDING ]]; then
    _INCANT_LAST_CMD=$_INCANT_PENDING
    _INCANT_LAST_STATUS=$st
    _INCANT_PENDING=""
  fi
}

autoload -Uz add-zsh-hook
add-zsh-hook preexec _incant_preexec
add-zsh-hook precmd _incant_precmd

# Opt-in stderr capture: while a command runs, the shell's stderr goes through
# tee into a private file (mode 600, removed on exit) as well as the terminal.
if [[ -n $INCANT_CAPTURE_STDERR ]]; then
  typeset -g _INCANT_STDERR_FILE
  _INCANT_STDERR_FILE=$(umask 077; command mktemp "${TMPDIR:-/tmp}/incant-stderr.XXXXXX") || _INCANT_STDERR_FILE=""
  typeset -g _INCANT_SAVED_FD=""

  if [[ -n $_INCANT_STDERR_FILE ]]; then
    _incant_stderr_start() {
      : >| $_INCANT_STDERR_FILE
      exec {_INCANT_SAVED_FD}>&2
      exec 2> >(command tee -a -- "$_INCANT_STDERR_FILE" >&$_INCANT_SAVED_FD)
    }
    _incant_stderr_stop() {
      [[ -n $_INCANT_SAVED_FD ]] || return 0
      exec 2>&$_INCANT_SAVED_FD {_INCANT_SAVED_FD}>&-
      _INCANT_SAVED_FD=""
    }
    _incant_stderr_cleanup() { command rm -f -- "$_INCANT_STDERR_FILE" }
    add-zsh-hook zshexit _incant_stderr_cleanup
  fi
fi

# --------------------------------------------------------------------------
# Calling the binary
# --------------------------------------------------------------------------

_incant_call() {
  command incant \
    --shell zsh \
    --last-command "$_INCANT_LAST_CMD" \
    --last-status "$_INCANT_LAST_STATUS" \
    "$@"
}

# Runs incant with its two streams kept apart. stdout is the command (or, with
# --all, the alternates one per line) and nothing else, so it can go straight
# into the buffer; stderr carries the error on failure and the effect preview
# on success. Merging them would paste the preview into the command line.
#
# Sets REPLY to stdout and _INCANT_ERR to stderr, and returns incant's status.
_incant_capture() {
  local errf rc
  errf=$(command mktemp "${TMPDIR:-/tmp}/incant.XXXXXX") || return 1
  REPLY=$(_incant_call "$@" 2>"$errf")
  rc=$?
  _INCANT_ERR=$(<"$errf")
  command rm -f -- "$errf"
  return $rc
}

# The line currently shown, if it is an unedited incant suggestion.
_incant_showing() {
  (( ${#_INCANT_CANDIDATES} )) && [[ $BUFFER == "${_INCANT_CANDIDATES[_INCANT_INDEX]}" ]]
}

# --------------------------------------------------------------------------
# Widget: rewrite the current line in place
#
# Type the request at the prompt and press the keybind. The buffer is replaced
# by the command. No quotes to type, no command name, and — importantly — the
# line is never accepted, so nothing runs until you press Enter yourself.
#
# Pressing it again on an unedited suggestion shows the next alternate, which
# is how an ambiguous request gets answered without a question.
# --------------------------------------------------------------------------

incant-widget() {
  if _incant_showing && (( ${#_INCANT_CANDIDATES} > 1 )); then
    _INCANT_INDEX=$(( _INCANT_INDEX % ${#_INCANT_CANDIDATES} + 1 ))
    BUFFER=${_INCANT_CANDIDATES[_INCANT_INDEX]}
    CURSOR=${#BUFFER}
    local pv
    pv=$(command incant --shell zsh --check "$BUFFER" 2>/dev/null)
    zle -M "incant: alternate $_INCANT_INDEX/${#_INCANT_CANDIDATES}${pv:+   $pv}"
    return 0
  fi

  local query=$BUFFER
  if [[ -z ${query//[[:space:]]/} ]]; then
    zle -M "incant: type a request first"
    return 0
  fi

  zle -M "incant: thinking…"
  zle -R

  local out rc
  _incant_capture --all -- "$query"
  rc=$?
  out=$REPLY

  zle -M ""

  if (( rc != 0 )); then
    # Leave the buffer exactly as the user typed it: a failed incantation
    # must never cost them their request.
    zle -M "incant: ${_INCANT_ERR%%$'\n'*}"
    return 0
  fi

  _INCANT_LAST_QUERY=$query
  _INCANT_CANDIDATES=("${(@f)out}")
  _INCANT_INDEX=1
  BUFFER=$_INCANT_CANDIDATES[1]
  CURSOR=${#BUFFER}

  # The effect preview sits under the prompt until the next keypress, which
  # is exactly while the user is reading the command.
  local msg=$_INCANT_ERR
  if (( ${#_INCANT_CANDIDATES} > 1 )); then
    msg+="${msg:+   }(^X^I: 1/${#_INCANT_CANDIDATES})"
  fi
  [[ -n $msg ]] && zle -M "$msg"
  return 0
}

zle -N incant-widget
bindkey '^X^I' incant-widget

# Put back the request. Off an incant suggestion, the key keeps its usual
# meaning (undo), so binding it costs nothing.
incant-restore-widget() {
  if _incant_showing && [[ -n $_INCANT_LAST_QUERY ]]; then
    BUFFER=$_INCANT_LAST_QUERY
    CURSOR=${#BUFFER}
    _INCANT_CANDIDATES=()
    return 0
  fi
  zle undo
}

zle -N incant-restore-widget
bindkey '^X^U' incant-restore-widget

# Repair the previous failed command in place.
incant-fix-widget() {
  if [[ -z $_INCANT_LAST_CMD ]]; then
    zle -M "incant: no previous command to fix"
    return 0
  fi

  zle -M "incant: diagnosing…"
  zle -R

  local -a extra
  if [[ -n $_INCANT_STDERR_FILE && -s $_INCANT_STDERR_FILE ]]; then
    extra=(--last-stderr "$(command tail -c 4000 -- "$_INCANT_STDERR_FILE")")
  fi

  local out rc
  _incant_capture --fix "${extra[@]}"
  rc=$?
  out=$REPLY

  zle -M ""

  if (( rc != 0 )); then
    zle -M "incant: ${_INCANT_ERR%%$'\n'*}"
    return 0
  fi

  _INCANT_CANDIDATES=()
  BUFFER=$out
  CURSOR=${#BUFFER}
  [[ -n $_INCANT_ERR ]] && zle -M "$_INCANT_ERR"
  return 0
}

zle -N incant-fix-widget
bindkey '^X^F' incant-fix-widget

# Preview whatever is on the line, typed or pasted — no model involved.
incant-preview-widget() {
  if [[ -z ${BUFFER//[[:space:]]/} ]]; then
    zle -M "incant: nothing to preview"
    return 0
  fi
  local pv
  pv=$(command incant --shell zsh --check "$BUFFER" 2>&1)
  zle -M "${pv:-incant: no file changes detected}"
}

zle -N incant-preview-widget
bindkey '^X^P' incant-preview-widget

# --------------------------------------------------------------------------
# Function: explicit invocation
#
# `incant "get the total line count of the txt files here"` pushes the result
# onto the editor buffer stack, so it appears at the next prompt ready to run.
# --------------------------------------------------------------------------

incant() {
  # Subcommands and flags that produce their own output go straight through.
  case "$1" in
    shell-init|--check|--help|-h|--version|-V)
      command incant "$@"
      return $?
      ;;
    doctor)
      # Only the bare subcommand; "incant doctor the csv" is a request.
      if [[ $# -eq 1 || $2 == -* ]]; then
        command incant "$@"
        return $?
      fi
      ;;
  esac

  # stderr -- the error, or the effect preview -- goes straight to the
  # terminal; only stdout, the command itself, is pushed onto the buffer.
  local out rc
  out=$(_incant_call "$@")
  rc=$?
  (( rc != 0 )) && return $rc

  print -z -- "$out"
}
