## SSH Session

An SSH session wrapper that allows remote command execution and scp.

## Example
```golang
// Leave password as empty string if use public key auth method
s, err := NewSession("127.0.0.1:22", "username", "password")

c, err := s.Run("echo 'Hello world!'")
if err != nil {
    log.Fatal("error executing command")
}
c.TailLog()

// Support recursive dir copy
s.CopyTo("src_path", "remote_path")
```
