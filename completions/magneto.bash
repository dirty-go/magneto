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
		local IFS=$'\n' # one name per line: keep spaces inside names
		compopt -o filenames 2>/dev/null
		COMPREPLY=($(compgen -d -- "$cur"))
		return 0
		;;
	-magnet)
		# A magnet URI can't be completed, but a batch file can.
		local IFS=$'\n' # one name per line: keep spaces inside names
		compopt -o filenames 2>/dev/null # bash 4+: add "/" to dirs, escape names
		COMPREPLY=($(compgen -f -- "$cur"))
		return 0
		;;
	-no-seed)
		COMPREPLY=($(compgen -W "true false" -- "$cur"))
		return 0
		;;
	-parallel)
		# No sensible values to suggest (an integer).
		return 0
		;;
	esac

	COMPREPLY=($(compgen -W "$opts" -- "$cur"))
	return 0
}

complete -F _magneto magneto
