/*
 * /bin/sh in the sandbox's image. For the code under test (MH_TRACE in its
 * environment, set by mh-sandbox-run), bash, which reads BASH_ENV
 * (/usr/share/mh-sandbox/trace.bash): what every shell script runs, builtins
 * included, is traced, and the shell then turns POSIX, as sh. Invoked as sh,
 * bash would read no BASH_ENV, and dash has no trace to send elsewhere than
 * the script's own stderr. For everything else — root, the system — dash,
 * as before.
 *
 * Nothing more: no privilege (not setuid), no file read, the arguments passed
 * on untouched.
 */
#include <stdlib.h>
#include <unistd.h>

int main(int argc, char **argv)
{
	(void)argc;
	if (getenv("MH_TRACE") != NULL) {
		/* trace.bash turns POSIX for this shell alone, then unsets it */
		setenv("MH_SH", "1", 1);
		argv[0] = "bash";
		execv("/usr/bin/bash", argv);
	}
	argv[0] = "sh";
	execv("/usr/bin/dash", argv);
	return 127;
}
