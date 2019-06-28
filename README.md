## SSH Session

An SSH session wrapper that allows remote command execution and scp.

## Example
```golang
// Leave password as empty string if use public key auth method
s, err := NewSession("127.0.0.1:22", "username", "password", "alternative_private_key_path_other_than_.ssh/id_rsa", time.Minute /* connection dial timeout */)

c, err := s.Run("echo 'Hello world!'")
if err != nil {
    log.Fatal("error executing command")
}
for line := range c.CombinedOut() {
	fmt.Printf("%v\n", string(line))
}

// Support recursive dir copy
s.CopyTo("src_path", "remote_path")
```
