# BASH_ENV of the code under test (mh-sandbox-run): every non-interactive
# bash it starts reads this first — /bin/sh too, which is bash for the code
# (sh.c). It sends the shell's trace (set -x: each command as run, after
# expansion — builtins, assignments, what eval and a pipe into bash were
# given, decoded) to the sandbox's trace, a FIFO only mhsink reads
# (mh-sandbox-lib, tracer): the code can write to it, not read it back nor
# undo what it wrote. mh-sandbox-report reads it as data.
#
# Each shell opens the FIFO itself, on a descriptor few scripts touch, and
# the trace's settings are its own (not exported): a child whose
# descriptors were closed (Python's subprocess) opens it again, instead of
# inheriting a number that no longer points anywhere.
#
# What it changes for the code: $- has x; a script's own set -x goes to the
# trace, not its stderr; a shell started as sh is bash in POSIX mode with
# dash's echo (a function: type echo says so). Code that looks can tell: a signal, not a defence.
#
# The FIFO first: a redirection that fails ends a shell in POSIX mode.
__mh_t=
if [ -p /run/mh-sandbox/trace ] && { exec 1022>>/run/mh-sandbox/trace; } 2>/dev/null; then
	__mh_t=1
fi
if [ -n "${MH_SH-}" ]; then
	unset MH_SH
	set -o posix
	# dash's echo: escapes always, and -n (alone, first) its only option.
	# bash's in POSIX mode with xpg_echo takes no option at all: echo -n,
	# common in sh scripts, would print "-n". A case, which set -x does not
	# show: the trace has the echo and its printf, nothing more.
	echo() {
		case ${1-} in
		-n) shift; printf '%b' "$*" ;;
		*) printf '%b\n' "$*" ;;
		esac
	}
fi
if [ -n "$__mh_t" ]; then
	unset __mh_t
	BASH_XTRACEFD=1022
	PS4=$'+${EPOCHREALTIME}\t${0##*/}\t'
	set -x
fi
