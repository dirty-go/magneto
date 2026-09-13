#!/usr/bin/env bash
# Bash completion for magneto.
#
# Install by sourcing this file, e.g. add to ~/.bashrc:
#   source /path/to/magneto/completions/magneto.bash
#
# Or drop it into your system's bash-completion directory, e.g.:
#   cp completions/magneto.bash /etc/bash_completion.d/magneto
#   cp completions/magneto.bash "$(brew --prefix)/etc/bash_completion.d/magneto"  # Homebrew

_magneto() {
	local cur prev opts
	COMPREPLY=()
	cur=${COMP_WORDS[COMP_CWORD]}
	prev=${COMP_WORDS[COMP_CWORD - 1]}
	opts="-magnet -out -no-seed -parallel -h -help"

	case "$prev" in
	-out)
		COMPREPLY=($(compgen -d -- "$cur"))
		return 0
		;;
	-no-seed)
		COMPREPLY=($(compgen -W "true false" -- "$cur"))
		return 0
		;;
	-parallel | -magnet)
		# No sensible values to suggest (an integer, a magnet URI).
		return 0
		;;
	esac

	COMPREPLY=($(compgen -W "$opts" -- "$cur"))
	return 0
}

complete -F _magneto magneto
