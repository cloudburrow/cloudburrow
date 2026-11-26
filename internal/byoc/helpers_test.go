package byoc

import (
	"bufio"
	"strings"
)

func bufioScanner(s string) *bufio.Scanner { return bufio.NewScanner(strings.NewReader(s)) }
