package cmd

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	operator "github.com/alexellis/k3sup/pkg/operator"

	homedir "github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/terminal"
)

var kubeconfig []byte

type k3sExecOptions struct {
	Datastore    string
	ExtraArgs    string
	FlannelIPSec bool
	NoExtras     bool
}

// PinnedK3sChannel is a predictable K8s version for use
// with the examples in the README. You can override this via
// a flag, or give a specific version, see the command help message.
const PinnedK3sChannel = "v1.19"
const getScript = "curl -sfL https://get.k3s.io"

// MakeInstall creates the install command
func MakeInstall() *cobra.Command {
	var command = &cobra.Command{
		Use:   "install",
		Short: "Install k3s on a server via SSH",
		Long: `Install k3s on a server via SSH.

` + SupportMsg,
		Example: `  k3sup install --ip IP --user USER

  k3sup install --local --k3s-version v1.19.7

  k3sup install --ip IP --cluster

  k3sup install --ip IP --k3s-channel latest

  k3sup install --host HOST \
    --ssh-key $HOME/ec2-key.pem --user ubuntu`,
		SilenceUsage: true,
	}

	command.Flags().IP("ip", net.ParseIP("127.0.0.1"), "Public IP of node")
	command.Flags().String("user", "root", "Username for SSH login")

	command.Flags().String("host", "", "Public hostname of node on which to install agent")
	command.Flags().String("host-ip", "", "Public hostname of an existing k3s server")

	command.Flags().String("ssh-key", "~/.ssh/id_rsa", "The ssh key to use for remote login")
	command.Flags().Int("ssh-port", 22, "The port on which to connect for ssh")
	command.Flags().Bool("sudo", true, "Use sudo for installation. e.g. set to false when using the root user and no sudo is available.")
	command.Flags().Bool("skip-install", false, "Skip the k3s installer")
	command.Flags().Bool("print-config", false, "Print the kubeconfig obtained from the server after installation")

	command.Flags().String("local-path", "kubeconfig", "Local path to save the kubeconfig file")
	command.Flags().String("context", "default", "Set the name of the kubeconfig context.")
	command.Flags().Bool("no-extras", false, `Disable "servicelb" and "traefik"`)

	command.Flags().Bool("ipsec", false, "Enforces and/or activates optional extra argument for k3s: flannel-backend option: ipsec")
	command.Flags().Bool("merge", false, `Merge the config with existing kubeconfig if it already exists.
Provide the --local-path flag with --merge if a kubeconfig already exists in some other directory`)
	command.Flags().Bool("local", false, "Perform a local install without using ssh")
	command.Flags().Bool("cluster", false, "Form a cluster using embedded etcd (requires K8s >= 1.19)")

	command.Flags().Bool("print-command", false, "Print a command that you can use with SSH to manually recover from an error")
	command.Flags().String("datastore", "", "connection-string for the k3s datastore to enable HA - i.e. \"mysql://username:password@tcp(hostname:3306)/database-name\"")

	command.Flags().String("k3s-version", "", "Set a version to install, overrides k3s-channel")
	command.Flags().String("k3s-extra-args", "", "Additional arguments to pass to k3s installer, wrapped in quotes (e.g. --k3s-extra-args '--no-deploy servicelb')")
	command.Flags().String("k3s-channel", PinnedK3sChannel, "Release channel: stable, latest, or i.e. v1.19")

	command.Flags().String("tls-san", "", "Create a certificate for an additional IP or hostname")

	command.PreRunE = func(command *cobra.Command, args []string) error {
		_, err := command.Flags().GetIP("ip")
		if err != nil {
			return err
		}
		_, err = command.Flags().GetIP("host")
		if err != nil {
			return err
		}
		return nil
	}

	command.RunE = func(command *cobra.Command, args []string) error {

		fmt.Printf("Running: k3sup install\n")

		localKubeconfig, _ := command.Flags().GetString("local-path")

		skipInstall, err := command.Flags().GetBool("skip-install")
		if err != nil {
			return err
		}

		tlsSAN, _ := command.Flags().GetString("tls-san")

		useSudo, err := command.Flags().GetBool("sudo")
		if err != nil {
			return err
		}

		printConfig, err := command.Flags().GetBool("print-config")
		if err != nil {
			return err
		}

		sudoPrefix := ""
		if useSudo {
			sudoPrefix = "sudo "
		}

		k3sVersion, err := command.Flags().GetString("k3s-version")
		if err != nil {
			return err
		}
		k3sExtraArgs, err := command.Flags().GetString("k3s-extra-args")
		if err != nil {
			return err
		}
		k3sChannel, err := command.Flags().GetString("k3s-channel")
		if err != nil {
			return err
		}
		k3sNoExtras, err := command.Flags().GetBool("no-extras")
		if err != nil {
			return err
		}

		flannelIPSec, _ := command.Flags().GetBool("ipsec")

		local, _ := command.Flags().GetBool("local")

		ip, err := command.Flags().GetIP("ip")
		if err != nil {
			return err
		}
		host, err := command.Flags().GetString("host")
		if err != nil {
			return err
		}
		if len(host) == 0 {
			host = ip.String()
		}
		log.Println(host)

		cluster, _ := command.Flags().GetBool("cluster")
		datastore, _ := command.Flags().GetString("datastore")
		printCommand, err := command.Flags().GetBool("print-command")
		if err != nil {
			return err
		}

		merge, err := command.Flags().GetBool("merge")
		if err != nil {
			return err
		}
		context, err := command.Flags().GetString("context")
		if err != nil {
			return err
		}

		if len(datastore) > 0 {
			if strings.Index(datastore, "ssl-mode=REQUIRED") > -1 {
				return fmt.Errorf("remove ssl-mode=REQUIRED from your datastore string, it is not supported by the k3s syntax")
			}
			if strings.Index(datastore, "mysql") > -1 && strings.Index(datastore, "tcp") == -1 {
				return fmt.Errorf("you must specify the mysql host as tcp(host:port) or tcp(ip:port), see the k3s docs for more: https://rancher.com/docs/k3s/latest/en/installation/ha")
			}
		}

		installk3sExec := makeInstallExec(cluster, host, tlsSAN,
			k3sExecOptions{
				Datastore:    datastore,
				FlannelIPSec: flannelIPSec,
				NoExtras:     k3sNoExtras,
				ExtraArgs:    k3sExtraArgs,
			})

		if len(k3sVersion) == 0 && len(k3sChannel) == 0 {
			return fmt.Errorf("give a value for --k3s-version or --k3s-channel")
		}

		installStr := createVersionStr(k3sVersion, k3sChannel)

		installK3scommand := fmt.Sprintf("%s | %s %s sh -\n", getScript, installk3sExec, installStr)

		getConfigcommand := fmt.Sprintf(sudoPrefix + "cat /etc/rancher/k3s/k3s.yaml\n")

		if local {
			operator := operator.ExecOperator{}

			fmt.Printf("Executing: %s\n", installK3scommand)

			res, err := operator.Execute(installK3scommand)
			if err != nil {
				return err
			}

			if len(res.StdErr) > 0 {
				fmt.Printf("stderr: %q", res.StdErr)
			}
			if len(res.StdOut) > 0 {
				fmt.Printf("stdout: %q", res.StdOut)
			}

			if err = obtainKubeconfig(operator, getConfigcommand, host, context, localKubeconfig, merge, printConfig); err != nil {
				return err
			}

			return nil
		}

		port, _ := command.Flags().GetInt("ssh-port")

		fmt.Println("Public IP: " + host)

		user, _ := command.Flags().GetString("user")
		sshKey, _ := command.Flags().GetString("ssh-key")

		sshKeyPath := expandPath(sshKey)

		authMethod, closeSSHAgent, err := loadPublickey(sshKeyPath)
		if err != nil {
			return errors.Wrapf(err, "unable to load the ssh key with path %q", sshKeyPath)
		}

		defer closeSSHAgent()

		config := &ssh.ClientConfig{
			User: user,
			Auth: []ssh.AuthMethod{
				authMethod,
			},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		}

		address := fmt.Sprintf("%s:%d", host, port)
		operator, err := operator.NewSSHOperator(address, config)

		if err != nil {
			return errors.Wrapf(err, "unable to connect to %s over ssh", address)
		}

		defer operator.Close()

		if !skipInstall {

			if printCommand {
				fmt.Printf("ssh: %s\n", installK3scommand)
			}

			res, err := operator.Execute(installK3scommand)

			if err != nil {
				return fmt.Errorf("error received processing command: %s", err)
			}

			fmt.Printf("Result: %s %s\n", string(res.StdOut), string(res.StdErr))
		}

		if printCommand {
			fmt.Printf("ssh: %s\n", getConfigcommand)
		}
		if err = obtainKubeconfig(operator, getConfigcommand, host, context, localKubeconfig, merge, printConfig); err != nil {
			return err
		}

		return nil
	}

	command.PreRunE = func(command *cobra.Command, args []string) error {
		if _, err := command.Flags().GetIP("ip"); err != nil {
			return err
		}

		if _, err := command.Flags().GetInt("ssh-port"); err != nil {
			return err
		}

		return nil
	}
	return command
}

func obtainKubeconfig(operator operator.CommandOperator, getConfigcommand, host, context, localKubeconfig string, merge, printConfig bool) error {
	res, err := operator.ExecuteStdio(getConfigcommand, false)
	if err != nil {
		return fmt.Errorf("error received processing command: %s", err)
	}

	if printConfig {
		fmt.Printf("Result: %s %s\n", string(res.StdOut), string(res.StdErr))
	}

	absPath, _ := filepath.Abs(localKubeconfig)

	kubeconfig := rewriteKubeconfig(string(res.StdOut), host, context)

	if merge {
		// Create a merged kubeconfig
		kubeconfig, err = mergeConfigs(absPath, context, []byte(kubeconfig))
		if err != nil {
			return err
		}
	}

	// Create a new kubeconfig
	if err := writeConfig(absPath, []byte(kubeconfig), context, false); err != nil {
		return err
	}

	return nil
}

// Generates config files give the path to file: string and the data: []byte
func writeConfig(path string, data []byte, context string, suppressMessage bool) error {
	absPath, _ := filepath.Abs(path)
	if !suppressMessage {
		fmt.Printf(`Saving file to: %s

# Test your cluster with:
export KUBECONFIG=%s
kubectl config use-context %s
kubectl get node -o wide
`,
			absPath,
			absPath,
			context)
	}

	if err := ioutil.WriteFile(absPath, []byte(data), 0600); err != nil {
		return err
	}

	return nil
}

func mergeConfigs(localKubeconfigPath, context string, k3sconfig []byte) ([]byte, error) {
	// Create a temporary kubeconfig to store the config of the newly create k3s cluster
	file, err := ioutil.TempFile(os.TempDir(), "k3s-temp-*")
	if err != nil {
		return nil, fmt.Errorf("Could not generate a temporary file to store the kuebeconfig: %s", err)
	}
	defer file.Close()

	if err := writeConfig(file.Name(), []byte(k3sconfig), context, true); err != nil {
		return nil, err
	}

	fmt.Printf("Merging with existing kubeconfig at %s\n", localKubeconfigPath)

	// Append KUBECONFIGS in ENV Vars
	appendKubeConfigENV := fmt.Sprintf("KUBECONFIG=%s:%s", localKubeconfigPath, file.Name())

	// Merge the two kubeconfigs and read the output into 'data'
	cmd := exec.Command("kubectl", "config", "view", "--merge", "--flatten")
	cmd.Env = append(os.Environ(), appendKubeConfigENV)
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Could not merge kubeconfigs: %s", err)
	}

	// Remove the temporarily generated file
	err = os.Remove(file.Name())
	if err != nil {
		return nil, errors.Wrapf(err, "Could not remove temporary kubeconfig file: %s", file.Name())
	}

	return data, nil
}

func expandPath(path string) string {
	res, _ := homedir.Expand(path)
	return res
}

func sshAgent(publicKeyPath string) (ssh.AuthMethod, func() error) {
	if sshAgentConn, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK")); err == nil {
		sshAgent := agent.NewClient(sshAgentConn)

		keys, _ := sshAgent.List()
		if len(keys) == 0 {
			return nil, sshAgentConn.Close
		}

		pubkey, err := ioutil.ReadFile(publicKeyPath)
		if err != nil {
			return nil, sshAgentConn.Close
		}

		authkey, _, _, _, err := ssh.ParseAuthorizedKey(pubkey)
		if err != nil {
			return nil, sshAgentConn.Close
		}
		parsedkey := authkey.Marshal()

		for _, key := range keys {
			if bytes.Equal(key.Blob, parsedkey) {
				return ssh.PublicKeysCallback(sshAgent.Signers), sshAgentConn.Close
			}
		}
	}
	return nil, func() error { return nil }
}

func loadPublickey(path string) (ssh.AuthMethod, func() error, error) {
	noopCloseFunc := func() error { return nil }

	key, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, noopCloseFunc, fmt.Errorf("unable to read file: %s, %s", path, err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		if _, ok := err.(*ssh.PassphraseMissingError); !ok {
			return nil, noopCloseFunc, fmt.Errorf("unable to parse private key: %s", err.Error())
		}

		agent, close := sshAgent(path + ".pub")
		if agent != nil {
			return agent, close, nil
		}

		defer close()

		fmt.Printf("Enter passphrase for '%s': ", path)
		STDIN := int(os.Stdin.Fd())
		bytePassword, _ := terminal.ReadPassword(STDIN)

		// Ignore any error from reading stdin to retain existing behaviour for unit test in
		// install_test.go

		// if err != nil {
		// 	return nil, noopCloseFunc, fmt.Errorf("reading password from stdin failed: %s", err.Error())
		// }

		fmt.Println()

		signer, err = ssh.ParsePrivateKeyWithPassphrase(key, bytePassword)
		if err != nil {
			return nil, noopCloseFunc, fmt.Errorf("parse private key with passphrase failed: %s", err)
		}
	}

	return ssh.PublicKeys(signer), noopCloseFunc, nil
}

func rewriteKubeconfig(kubeconfig string, host string, context string) []byte {
	if context == "" {
		context = "default"
	}

	kubeconfigReplacer := strings.NewReplacer(
		"127.0.0.1", host,
		"localhost", host,
		"default", context,
	)

	return []byte(kubeconfigReplacer.Replace(kubeconfig))
}

func makeInstallExec(cluster bool, host, tlsSAN string, options k3sExecOptions) string {
	extraArgs := []string{}
	if len(options.Datastore) > 0 {
		extraArgs = append(extraArgs, fmt.Sprintf("--datastore-endpoint %s", options.Datastore))
	}
	if options.FlannelIPSec {
		extraArgs = append(extraArgs, "--flannel-backend ipsec")
	}

	if options.NoExtras {
		extraArgs = append(extraArgs, "--no-deploy servicelb")
		extraArgs = append(extraArgs, "--no-deploy traefik")
	}

	extraArgs = append(extraArgs, options.ExtraArgs)
	extraArgsCmdline := ""
	for _, a := range extraArgs {
		extraArgsCmdline += a + " "
	}

	installExec := "INSTALL_K3S_EXEC='server"
	if cluster {
		installExec += " --cluster-init"
	}

	san := host
	if len(tlsSAN) > 0 {
		san = tlsSAN
	}
	installExec += fmt.Sprintf(" --tls-san %s", san)

	if trimmed := strings.TrimSpace(extraArgsCmdline); len(trimmed) > 0 {
		installExec += fmt.Sprintf(" %s", trimmed)
	}

	installExec += "'"

	return installExec
}
