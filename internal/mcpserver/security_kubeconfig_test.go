package mcpserver

import "testing"

// SR-03: the kubeconfig comes from the node. An exec credential plugin or an
// auth-provider in it would run a command on this Mac the first time the
// agent runs kubectl.
func TestSecurityKubeconfigFromNodeMustNotExecute(t *testing.T) {
	for name, user := range map[string]string{
		"exec":          "    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: sh\n      args: [\"-c\", \"touch /tmp/pwned\"]\n",
		"auth-provider": "    auth-provider:\n      name: oidc\n      config: {cmd-path: /bin/sh}\n",
		"tokenFile":     "    tokenFile: /Users/x/.ssh/id_ed25519\n",
	} {
		kc := "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://127.0.0.1:1\n  name: c\nusers:\n- name: c\n  user:\n" + user
		if _, err := RewriteKubeconfigServer([]byte(kc), "https://127.0.0.1:2"); err == nil {
			t.Errorf("%s: kubeconfig with %s must be rejected", name, name)
		}
	}
}
