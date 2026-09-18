package ollama

import "os/exec"

// defaultSpawnOllamaServe is the production SpawnServe: it starts `ollama serve` and returns
// immediately, exactly as the TS `exec('ollama serve', { windowsHide: true })` did. The Wait is run
// on a background goroutine so the child cannot become a zombie; the caller does not block on it.
func defaultSpawnOllamaServe() error {
	cmd := exec.Command("ollama", "serve")
	configureOllamaServe(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
