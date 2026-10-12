// This build-time smoke program never writes request text or audio to disk.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func run() error {
	helper := flag.String("helper", "", "helper executable")
	dir := flag.String("dir", "", "engine root containing vits-piper-it_IT-miro-high")
	language := flag.String("language", "italian", "italian or english")
	threads := flag.Int("threads", 2, "CPU threads")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, *helper, "--dir", *dir, "--language", *language, "--threads", strconv.Itoa(*threads))
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	r := bufio.NewReaderSize(out, 65537)
	readHeader := func() (map[string]any, error) {
		line, err := r.ReadSlice('\n')
		if err != nil || len(line) > 65536 {
			return nil, fmt.Errorf("invalid protocol header: %w", err)
		}
		var h map[string]any
		if err := json.Unmarshal(line, &h); err != nil {
			return nil, err
		}
		return h, nil
	}
	ready, err := readHeader()
	if err != nil {
		return err
	}
	if ready["type"] != "ready" || ready["protocol"] != float64(1) || ready["sample_rate"] != float64(24000) {
		return fmt.Errorf("unexpected readiness: %v", ready)
	}
	fmt.Printf("Piper %s resident startup: %.3fs\n", *language, time.Since(start).Seconds())
	phrases := []string{"Ciao, questa è la prima risposta. Il servizio funziona correttamente.", "Seconda risposta: controlla il gateway, poi esegui il comando git status."}
	if *language == "english" {
		phrases = []string{"The gateway is ready. Please check the deployment status.", "Second reply: the HTTP request completed successfully. Run git status."}
	}
	for i, phrase := range phrases {
		id := strconv.Itoa(i + 1)
		start = time.Now()
		if err := json.NewEncoder(in).Encode(map[string]string{"id": id, "text": phrase}); err != nil {
			return err
		}
		header, err := readHeader()
		if err != nil {
			return err
		}
		count, ok := header["sample_count"].(float64)
		if !ok || count != math.Trunc(count) || count < 2400 || count > 24000*600 || header["id"] != id || header["sample_rate"] != float64(24000) || header["error"] != nil {
			return fmt.Errorf("invalid response: %v", header)
		}
		var bytes [4096]byte
		peak, sum := float64(0), float64(0)
		for left := int(count) * 4; left > 0; {
			n := min(left, len(bytes))
			if _, err := io.ReadFull(r, bytes[:n]); err != nil {
				return err
			}
			for j := 0; j < n; j += 4 {
				s := float64(math.Float32frombits(binary.LittleEndian.Uint32(bytes[j:])))
				if math.IsNaN(s) || math.IsInf(s, 0) || math.Abs(s) > 1 {
					return fmt.Errorf("invalid float32 PCM")
				}
				peak = max(peak, math.Abs(s))
				sum += s * s
			}
			clear(bytes[:n])
			left -= n
		}
		if peak < 0.01 || sum/count < 0.000001 {
			return fmt.Errorf("silent output")
		}
		fmt.Printf("Piper %s request %s: %d samples, %.3fs audio, %.3fs generation, peak %.3f\n", *language, id, int(count), count/24000, time.Since(start).Seconds(), peak)
	}
	if err := in.Close(); err != nil {
		return err
	}
	if extra, err := r.ReadByte(); err != io.EOF {
		return fmt.Errorf("unexpected output after EOF (%d): %v", extra, err)
	}
	return cmd.Wait()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
