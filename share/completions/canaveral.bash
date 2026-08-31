# bash completion for canaveral.
#
#   source /path/to/canaveral.bash
#
# All the intelligence is in `canaveral complete`, which the launcher popup
# calls too — so the two can never disagree about what a half-typed line means.
# This file only marshals bash's variables into it and its answers back out.

_canaveral() {
	local line out=()

	# COMP_WORDS[0] is the command itself. Everything from 1 up to and
	# including the word under the cursor is the line to complete; when the
	# cursor sits after a space that last element is the empty string, which is
	# exactly how `complete` tells "starting a new argument" from "still typing
	# the last one".
	mapfile -t out < <(canaveral complete --format=lines -- "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null)

	COMPREPLY=()
	for line in "${out[@]}"; do
		# -o nospace is on for the sake of namespaces, whose trailing "/" must
		# not end the word — there is always more to type after it. Everything
		# else has to add its own space back. A trailing slash is the only
		# thing `complete` marks as continuing, so it doubles as the test.
		[[ $line == */ ]] || line+=" "
		COMPREPLY+=("$line")
	done
}

complete -o nospace -F _canaveral canaveral

# The `cv` wrapper documented in the README, if you use it, wants the same
# completion: it is the same command with the same arguments.
if declare -F cv >/dev/null 2>&1; then
	complete -o nospace -F _canaveral cv
fi
