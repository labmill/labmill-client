#!/bin/sh
set -eu
cat > "$1/hello.c" <<'EOC'
#include <omp.h>
#include <stdio.h>

int main(void) {
	int n = 0;
	#pragma omp parallel
	{
		#pragma omp atomic
		n++;
	}
	printf("threads %d\n", n);
	return 0;
}
EOC
