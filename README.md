## SSH Session

An SSH session wrapper that allows executing multiple commands within the same session. Execution output from stdout and stderr are streaming through channel.

## Example
```
// leave password as empty string if use public key auth method
s, err := NewSession("127.0.0.1:22", "username", "password")

outCh, errCh, err := s.Run("echo 'Hello world!'")
if err != nil {
    log.Fatal("error executing command")
}

for line := range outCh {
	log.Print(string(line))
}

for line := range errCh {
	log.Print(string(line))
}
```
