# Renders demo/demo.gif with https://github.com/charmbracelet/vhs:
#   brew install vhs && vhs demo/demo.tape
# Needs incant on PATH with working credentials (incant doctor).
Output demo/demo.gif

Set Shell zsh
Set FontSize 20
Set Width 1200
Set Height 460
Set Padding 24
Set TypingSpeed 45ms

Hide
Type `cd "$(demo/setup.sh)" && eval "$(incant shell-init zsh)" && PS1='%F{blue}%1~%f ❯ ' && clear`
Enter
Show

# 1. Ask in plain English, press ^X^I: the line becomes a command, and the
#    preview says what it would do here.
Type "delete the log files older than a week"
Sleep 600ms
Ctrl+X
Ctrl+I
Sleep 5s

# 2. Press again for an alternate.
Ctrl+X
Ctrl+I
Sleep 3s

# 3. Put the request back.
Ctrl+X
Ctrl+U
Sleep 1500ms
Ctrl+U

# 4. Preview any command, even one you typed: incant catches the classic
#    mistakes without running anything.
Type "find . -name node_modules -delete"
Sleep 400ms
Ctrl+X
Ctrl+P
Sleep 3s
Ctrl+U
Type "sort scores.txt > scores.txt"
Sleep 400ms
Ctrl+X
Ctrl+P
Sleep 3500ms
