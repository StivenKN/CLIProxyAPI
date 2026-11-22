// Constructors live in this file, not in the cgo preamble of main.go: main.go
// uses //export, so cgo compiles its preamble into two translation units and
// every static constructor there would run twice per dlopen.

#include <stdlib.h>

#ifndef PLUGIN_VERSION
#define PLUGIN_VERSION 1
#endif

#if PLUGIN_VERSION == 4
#include <signal.h>
__attribute__((constructor)) static void cliproxyTestSigillOnDlopen(void) {
	raise(SIGILL);
}
#elif PLUGIN_VERSION == 5
#include <unistd.h>
__attribute__((constructor)) static void cliproxyTestPrematureExitZeroOnDlopen(void) {
	_exit(0);
}
#elif PLUGIN_VERSION == 6
#include <signal.h>
__attribute__((constructor)) static void cliproxyTestSigkillOnDlopen(void) {
	raise(SIGKILL);
}
#elif PLUGIN_VERSION == 7
#include <stdio.h>
#include <unistd.h>
__attribute__((constructor)) static void cliproxyTestInheritedStdioGrandchildOnDlopen(void) {
	pid_t pid = fork();
	if (pid == 0) {
		pause();
		_exit(0);
	}
	if (pid > 0) {
		const char* pid_path = getenv("CLIPROXY_TEST_GRANDCHILD_PID_FILE");
		if (pid_path != NULL && pid_path[0] != '\0') {
			FILE* fp = fopen(pid_path, "w");
			if (fp != NULL) {
				fprintf(fp, "%d\n", (int)pid);
				fclose(fp);
			}
		}
	}
}
#endif
