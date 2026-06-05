//go:build !windows

package main

import (
	"fmt"
	"io"
)

// newWindowsPCMPlayer only has a real implementation on Windows. This stub
// exists so main.go compiles on every platform; it is never reached because
// the playback switch only calls it under runtime.GOOS == "windows".
func newWindowsPCMPlayer(sampleRate, channels, bitsPerSample int) (io.WriteCloser, error) {
	return nil, fmt.Errorf("WinMM PCM playback is only available on Windows")
}
