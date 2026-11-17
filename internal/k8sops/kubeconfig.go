package k8sops

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// kubeconfigFile is the subset of the client-go kubeconfig API this package
// reads. Exec and auth-provider credentials are deliberately not executed: the
// process would have to spawn a helper binary, which the deployment does not
// ship; they are rejected with an actionable Chinese configuration error.
type kubeconfigFile struct {
	CurrentContext string              `yaml:"current-context"`
	Clusters       []kubeconfigCluster `yaml:"clusters"`
	Users          []kubeconfigUser    `yaml:"users"`
	Contexts       []kubeconfigContext `yaml:"contexts"`
}

type kubeconfigCluster struct {
	Name    string `yaml:"name"`
	Cluster struct {
		Server                   string `yaml:"server"`
		CertificateAuthority     string `yaml:"certificate-authority"`
		CertificateAuthorityData string `yaml:"certificate-authority-data"`
		InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
	} `yaml:"cluster"`
}

type kubeconfigUser struct {
	Name string `yaml:"name"`
	User struct {
		Token                 string `yaml:"token"`
		TokenFile             string `yaml:"tokenFile"`
		ClientCertificate     string `yaml:"client-certificate"`
		ClientCertificateData string `yaml:"client-certificate-data"`
		ClientKey             string `yaml:"client-key"`
		ClientKeyData         string `yaml:"client-key-data"`
		Username              string `yaml:"username"`
		Password              string `yaml:"password"`
		// Exec and AuthProvider are pointer-typed so merely declaring the
		// plugin section is detected even when it has no fields.
		Exec         *kubeconfigPlugin `yaml:"exec"`
		AuthProvider *kubeconfigPlugin `yaml:"auth-provider"`
	} `yaml:"user"`
}

type kubeconfigPlugin struct {
	Command string `yaml:"command"`
	Name    string `yaml:"name"`
}

type kubeconfigContext struct {
	Name    string `yaml:"name"`
	Context struct {
		Cluster   string `yaml:"cluster"`
		User      string `yaml:"user"`
		Namespace string `yaml:"namespace"`
	} `yaml:"context"`
}

// settingsFromKubeconfig parses one kubeconfig file and selects the cluster and
// user referenced by the requested context (or current-context). Every failure
// is a configuration error: the operator explicitly pointed at a file, so a
// silent degrade to "not configured" would hide a broken mount.
func settingsFromKubeconfig(path, contextOverride string) (Settings, error) {
	path = strings.TrimSpace(path)
	raw, err := os.ReadFile(path)
	if err != nil {
		return Settings{}, fmt.Errorf("无法读取 kubeconfig 文件 %s: %w", path, err)
	}
	var config kubeconfigFile
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return Settings{}, fmt.Errorf("kubeconfig 文件 %s 格式错误: %w", path, err)
	}
	contextName := strings.TrimSpace(contextOverride)
	if contextName == "" {
		contextName = strings.TrimSpace(config.CurrentContext)
	}
	if contextName == "" {
		return Settings{}, fmt.Errorf("kubeconfig 文件 %s 未设置 current-context，请使用 RAG_K8S_CONTEXT 指定要使用的 context", path)
	}
	context, ok := findKubeconfigContext(config.Contexts, contextName)
	if !ok {
		return Settings{}, fmt.Errorf("kubeconfig 文件 %s 中不存在 context %q，请检查 RAG_K8S_CONTEXT", path, contextName)
	}
	cluster, ok := findKubeconfigCluster(config.Clusters, context.Context.Cluster)
	if !ok {
		return Settings{}, fmt.Errorf("kubeconfig context %q 引用的 cluster %q 不存在", contextName, context.Context.Cluster)
	}
	server := strings.TrimSpace(cluster.Cluster.Server)
	if server == "" {
		return Settings{}, fmt.Errorf("kubeconfig context %q 的 cluster %q 未配置 server", contextName, context.Context.Cluster)
	}

	settings := Settings{
		APIEndpoint:           server,
		Source:                SourceKubeconfig,
		KubeconfigPath:        path,
		ContextName:           contextName,
		InsecureSkipTLSVerify: cluster.Cluster.InsecureSkipTLSVerify,
	}

	caData, err := kubeconfigCABundle(path, cluster)
	if err != nil {
		return Settings{}, err
	}
	settings.CAData = caData

	userName := strings.TrimSpace(context.Context.User)
	if userName != "" {
		user, ok := findKubeconfigUser(config.Users, userName)
		if !ok {
			return Settings{}, fmt.Errorf("kubeconfig context %q 引用的 user %q 不存在", contextName, userName)
		}
		if err := applyKubeconfigUser(&settings, path, user); err != nil {
			return Settings{}, err
		}
	}

	namespace := strings.TrimSpace(context.Context.Namespace)
	if namespace == "" {
		namespace = "default"
	}
	deploymentNamespace := strings.TrimSpace(os.Getenv("RAG_K8S_DEPLOYMENT_NAMESPACE"))
	if deploymentNamespace == "" {
		deploymentNamespace = namespace
	}
	settings.Namespace = deploymentNamespace
	settings.DeploymentNamespace = deploymentNamespace
	settings.DocumentWorkerDeployment = strings.TrimSpace(os.Getenv("RAG_K8S_WORKER_DEPLOYMENT"))
	if settings.DocumentWorkerDeployment == "" {
		settings.DocumentWorkerDeployment = "rag-document-worker"
	}
	return settings, nil
}

func findKubeconfigContext(contexts []kubeconfigContext, name string) (kubeconfigContext, bool) {
	for _, context := range contexts {
		if strings.TrimSpace(context.Name) == name {
			return context, true
		}
	}
	return kubeconfigContext{}, false
}

func findKubeconfigCluster(clusters []kubeconfigCluster, name string) (kubeconfigCluster, bool) {
	for _, cluster := range clusters {
		if strings.TrimSpace(cluster.Name) == strings.TrimSpace(name) {
			return cluster, true
		}
	}
	return kubeconfigCluster{}, false
}

func findKubeconfigUser(users []kubeconfigUser, name string) (kubeconfigUser, bool) {
	for _, user := range users {
		if strings.TrimSpace(user.Name) == name {
			return user, true
		}
	}
	return kubeconfigUser{}, false
}

// kubeconfigCABundle resolves the cluster CA: inline base64 data wins over the
// referenced file, matching client-go. Both forms are optional (a publicly
// trusted endpoint works without a custom CA).
func kubeconfigCABundle(configPath string, cluster kubeconfigCluster) ([]byte, error) {
	if data := strings.TrimSpace(cluster.Cluster.CertificateAuthorityData); data != "" {
		decoded, err := decodeKubeconfigBase64("certificate-authority-data", data)
		if err != nil {
			return nil, err
		}
		return decoded, nil
	}
	reference := kubeconfigRelativePath(configPath, cluster.Cluster.CertificateAuthority)
	if reference == "" {
		return nil, nil
	}
	ca, err := os.ReadFile(reference)
	if err != nil {
		return nil, fmt.Errorf("无法读取 kubeconfig 引用的 CA 文件 %s: %w", reference, err)
	}
	return ca, nil
}

// applyKubeconfigUser resolves the user's static credentials. Token files are
// not read here: the file is re-read on every request so kubelet-style
// rotation keeps working. Client certificate files are static and therefore
// loaded once.
func applyKubeconfigUser(settings *Settings, configPath string, user kubeconfigUser) error {
	if user.User.Exec != nil {
		command := strings.TrimSpace(user.User.Exec.Command)
		if command == "" {
			command = "未指定命令"
		}
		return fmt.Errorf("kubeconfig 用户 %q 使用了 exec 插件（%s），当前版本不支持；请改用静态 token 或客户端证书", user.Name, command)
	}
	if user.User.AuthProvider != nil {
		name := strings.TrimSpace(user.User.AuthProvider.Name)
		if name == "" {
			name = "未指定名称"
		}
		return fmt.Errorf("kubeconfig 用户 %q 使用了 auth-provider 插件（%s），当前版本不支持；请改用静态 token 或客户端证书", user.Name, name)
	}
	token := strings.TrimSpace(user.User.Token)
	tokenFile := kubeconfigRelativePath(configPath, user.User.TokenFile)
	if token != "" && tokenFile != "" {
		// A token file is the rotating form; prefer it over a stale inline
		// token, exactly like client-go's auth plugins.
		token = ""
	}
	settings.Token = token
	settings.TokenFile = tokenFile
	settings.Username = strings.TrimSpace(user.User.Username)
	settings.Password = user.User.Password

	certData, keyData, err := kubeconfigClientCertificate(configPath, user)
	if err != nil {
		return err
	}
	settings.ClientCertData = certData
	settings.ClientKeyData = keyData
	return nil
}

func kubeconfigClientCertificate(configPath string, user kubeconfigUser) ([]byte, []byte, error) {
	certData := strings.TrimSpace(user.User.ClientCertificateData)
	keyData := strings.TrimSpace(user.User.ClientKeyData)
	certFile := kubeconfigRelativePath(configPath, user.User.ClientCertificate)
	keyFile := kubeconfigRelativePath(configPath, user.User.ClientKey)

	if certData != "" {
		decoded, err := decodeKubeconfigBase64("client-certificate-data", certData)
		if err != nil {
			return nil, nil, err
		}
		certData = string(decoded)
		certFile = ""
	}
	if keyData != "" {
		decoded, err := decodeKubeconfigBase64("client-key-data", keyData)
		if err != nil {
			return nil, nil, err
		}
		keyData = string(decoded)
		keyFile = ""
	}
	var certificate, key []byte
	if certData != "" {
		certificate = []byte(certData)
	} else if certFile != "" {
		loaded, err := os.ReadFile(certFile)
		if err != nil {
			return nil, nil, fmt.Errorf("无法读取 kubeconfig 引用的客户端证书 %s: %w", certFile, err)
		}
		certificate = loaded
	}
	if keyData != "" {
		key = []byte(keyData)
	} else if keyFile != "" {
		loaded, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("无法读取 kubeconfig 引用的客户端私钥 %s: %w", keyFile, err)
		}
		key = loaded
	}
	if len(certificate) == 0 && len(key) == 0 {
		return nil, nil, nil
	}
	if len(certificate) == 0 || len(key) == 0 {
		return nil, nil, fmt.Errorf("kubeconfig 用户 %q 需要同时配置 client-certificate 与 client-key（或对应的 -data 字段）", user.Name)
	}
	return certificate, key, nil
}

// kubeconfigRelativePath resolves the file references of a kubeconfig (CA,
// token file, client certificate) relative to the kubeconfig file itself, as
// client-go does.
func kubeconfigRelativePath(configPath, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(filepath.Dir(configPath), value)
}

func decodeKubeconfigBase64(field, value string) ([]byte, error) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, value)
	decoded, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig 字段 %s 不是合法的 base64: %w", field, err)
	}
	return decoded, nil
}
