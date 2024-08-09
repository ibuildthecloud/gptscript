package env

import (
	"fmt"
	"os"
	"strings"
)

func execEquals(bin, check string) bool {
	return bin == check ||
		bin == check+".exe"
}

func VarOrDefault(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}

	return defaultValue
}

func ToEnvLike(v string) string {
	v = strings.ReplaceAll(v, ".", "_")
	v = strings.ReplaceAll(v, "-", "_")
	return strings.ToUpper(v)
}

func Getenv(key string, envs []string) string {
	for i := len(envs) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(envs[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

func Matches(cmd []string, bin string) bool {
	switch len(cmd) {
	case 0:
		return false
	case 1:
		return execEquals(cmd[0], bin)
	}
	if cmd[0] == bin {
		return true
	}
	if cmd[0] == "/usr/bin/env" || cmd[0] == "/bin/env" {
		return execEquals(cmd[1], bin)
	}
	return false
}

func AppendPath(env []string, binPath string) []string {
	var newEnv []string
	for _, path := range env {
		v, ok := strings.CutPrefix(path, "PATH=")
		if ok {
			newEnv = append(newEnv, fmt.Sprintf("PATH=%s%s%s",
				binPath, string(os.PathListSeparator), v))
		} else if v, ok := strings.CutPrefix(path, "Path="); ok {
			newEnv = append(newEnv, fmt.Sprintf("Path=%s%s%s",
				binPath, string(os.PathListSeparator), v))
		}
	}
	return newEnv
}
