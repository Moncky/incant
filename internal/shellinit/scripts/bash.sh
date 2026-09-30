# incant — bash integration.
#
# Install with:   eval "$(incant shell-init bash)"     (in ~/.bashrc)
#
# The keybindings need bash 4 or newer, for bind -x and READLINE_LINE. macOS
# ships bash 3.2; `brew install bash` provides a current one. On an older bash
# only the explicit `incant` function is defined.
#
# Keys:
#   \C-x\C-i   turn the current line into a command; press again to cycle alternates
#   \C-x\C-u   put back the request you typed
#   \C-x\C-f   repair the previous failed command
#   \C-x\C-p   preview what the current line would change
#
# Unlike zsh, bash has no hook that runs just before a command, so stderr
# capture for fixes is not available here.

# --------------------------------------------------------------------------
# Outcome hook
#
# PROMPT_COMMAND runs before each prompt. The previous command is read back
# from history, and only counted when the history number moved, so pressing
# Enter on an empty line does not re-report the last failure.
#
# The variables are deliberately NOT exported: they hold the user's command
# history. The widgets pass them explicitly.
# --------------------------------------------------------------------------

_INCANT_LAST_CMD=""
_INCANT_LAST_STATUS=0
_INCANT_LAST_QUERY=""
_INCANT_HISTNUM=""
_INCANT_OUT=""
_INCANT_ERR=""
_INCANT_CANDIDATES=()
_INCANT_INDEX=0

_incant_prompt_hook() {
  # $? must be read first, and handed back so later PROMPT_COMMAND entries
  # see it unchanged.
  local st=$? h
  h=$(HISTTIMEFORMAT='' builtin history 1)
  if [[ $h =~ ^[[:space:]]*([0-9]+)[*[:space:]]+(.*)$ ]]; then
    if [[ ${BASH_REMATCH[1]} != "$_INCANT_HISTNUM" ]]; then
      _INCANT_HISTNUM=${BASH_REMATCH[1]}
      _INCANT_LAST_CMD=${BASH_REMATCH[2]}
      _INCANT_LAST_STATUS=$st
    fi
  fi
  return $st
}

case ";${PROMPT_COMMAND};" in
  *";_incant_prompt_hook;"*) ;;
  *) PROMPT_COMMAND="_incant_prompt_hook${PROMPT_COMMAND:+;$PROMPT_COMMAND}" ;;
esac

# --------------------------------------------------------------------------
# Calling the binary
# --------------------------------------------------------------------------

_incant_call() {
  command incant \
    --shell bash \
    --last-command "$_INCANT_LAST_CMD" \
    --last-status "$_INCANT_LAST_STATUS" \
    "$@"
}

# Runs incant with stdout and stderr kept apart: stdout is the command (or the
# alternates, one per line) and goes into the line; stderr is the error or the
# effect preview. Sets _INCANT_OUT and _INCANT_ERR; returns incant's status.
_incant_capture() {
  local errf rc
  errf=$(command mktemp "${TMPDIR:-/tmp}/incant.XXXXXX") || return 1
  _INCANT_OUT=$(_incant_call "$@" 2>"$errf")
  rc=$?
  _INCANT_ERR=$(<"$errf")
  command rm -f -- "$errf"
  return $rc
}

# bind -x functions cannot use a status line the way zle -M does; a message
# is printed above the prompt, which readline then redraws.
_incant_msg() { printf '%s\n' "$1" >&2; }

_incant_showing() {
  (( ${#_INCANT_CANDIDATES[@]} )) && [[ $READLINE_LINE == "${_INCANT_CANDIDATES[_INCANT_INDEX]}" ]]
}

_incant_set_line() {
  READLINE_LINE=$1
  READLINE_POINT=${#READLINE_LINE}
}

# --------------------------------------------------------------------------
# Widgets
# --------------------------------------------------------------------------

incant-widget() {
  if _incant_showing && (( ${#_INCANT_CANDIDATES[@]} > 1 )); then
    _INCANT_INDEX=$(( (_INCANT_INDEX + 1) % ${#_INCANT_CANDIDATES[@]} ))
    _incant_set_line "${_INCANT_CANDIDATES[_INCANT_INDEX]}"
    local pv
    pv=$(command incant --shell bash --check "$READLINE_LINE" 2>/dev/null)
    _incant_msg "incant: alternate $(( _INCANT_INDEX + 1 ))/${#_INCANT_CANDIDATES[@]}${pv:+   $pv}"
    return 0
  fi

  local query=$READLINE_LINE
  if [[ -z ${query//[[:space:]]/} ]]; then
    _incant_msg "incant: type a request first"
    return 0
  fi

  if ! _incant_capture --all -- "$query"; then
    # A failed incantation must never cost the user their request.
    _incant_msg "incant: ${_INCANT_ERR%%$'\n'*}"
    return 0
  fi

  _INCANT_LAST_QUERY=$query
  mapfile -t _INCANT_CANDIDATES <<< "$_INCANT_OUT"
  _INCANT_INDEX=0
  _incant_set_line "${_INCANT_CANDIDATES[0]}"

  local msg=$_INCANT_ERR
  if (( ${#_INCANT_CANDIDATES[@]} > 1 )); then
    msg+="${msg:+   }(\\C-x\\C-i: 1/${#_INCANT_CANDIDATES[@]})"
  fi
  [[ -n $msg ]] && _incant_msg "$msg"
  return 0
}

incant-restore-widget() {
  if _incant_showing && [[ -n $_INCANT_LAST_QUERY ]]; then
    _incant_set_line "$_INCANT_LAST_QUERY"
    _INCANT_CANDIDATES=()
  else
    _incant_msg "incant: nothing to put back"
  fi
}

incant-fix-widget() {
  if [[ -z $_INCANT_LAST_CMD ]]; then
    _incant_msg "incant: no previous command to fix"
    return 0
  fi
  if ! _incant_capture --fix; then
    _incant_msg "incant: ${_INCANT_ERR%%$'\n'*}"
    return 0
  fi
  _INCANT_CANDIDATES=()
  _incant_set_line "$_INCANT_OUT"
  [[ -n $_INCANT_ERR ]] && _incant_msg "$_INCANT_ERR"
  return 0
}

incant-preview-widget() {
  if [[ -z ${READLINE_LINE//[[:space:]]/} ]]; then
    _incant_msg "incant: nothing to preview"
    return 0
  fi
  local pv
  pv=$(command incant --shell bash --check "$READLINE_LINE" 2>&1)
  _incant_msg "${pv:-incant: no file changes detected}"
}

if (( BASH_VERSINFO[0] >= 4 )) && [[ $- == *i* ]]; then
  bind -x '"\C-x\C-i": incant-widget'
  bind -x '"\C-x\C-u": incant-restore-widget'
  bind -x '"\C-x\C-f": incant-fix-widget'
  bind -x '"\C-x\C-p": incant-preview-widget'
elif [[ $- == *i* ]]; then
  printf 'incant: bash %s is too old for the keybindings (need 4+; try: brew install bash). The incant function still works.\n' "$BASH_VERSION" >&2
fi

# --------------------------------------------------------------------------
# Function: explicit invocation
#
# bash cannot pre-fill the next prompt the way zsh's print -z does, so the
# command is printed and added to history: press Up to get it, ready to run.
# --------------------------------------------------------------------------

incant() {
  case "$1" in
    shell-init|--check|--help|-h|--version|-V)
      command incant "$@"
      return $?
      ;;
    doctor)
      if [[ $# -eq 1 || $2 == -* ]]; then
        command incant "$@"
        return $?
      fi
      ;;
  esac

  local out rc
  out=$(_incant_call "$@")
  rc=$?
  (( rc != 0 )) && return $rc

  builtin history -s -- "$out"
  printf '%s\n' "$out"
  printf 'incant: press Up to edit or run it\n' >&2
}
