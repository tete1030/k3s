package config

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/k3s-io/k3s/pkg/agent/containerd"
	"github.com/k3s-io/k3s/pkg/agent/proxy"
	agentutil "github.com/k3s-io/k3s/pkg/agent/util"
	"github.com/k3s-io/k3s/pkg/cli/cmds"
	"github.com/k3s-io/k3s/pkg/clientaccess"
	"github.com/k3s-io/k3s/pkg/daemons/config"
	"github.com/k3s-io/k3s/pkg/daemons/control/deps"
	"github.com/k3s-io/k3s/pkg/util"
	"github.com/k3s-io/k3s/pkg/version"
	"github.com/k3s-io/k3s/pkg/vpn"
	"github.com/pkg/errors"
	"github.com/rancher/wrangler/pkg/slice"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/json"
	utilsnet "k8s.io/utils/net"
)

const (
	DefaultPodManifestPath = "pod-manifests"
)

// Get returns a pointer to a completed Node configuration struct,
// containing a merging of the local CLI configuration with settings from the server.
// A call to this will bock until agent configuration is successfully returned by the
// server.
func Get(ctx context.Context, agent cmds.Agent, proxy proxy.Proxy) *config.Node {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
RETRY:
	for {
		agentConfig, err := get(ctx, &agent, proxy)
		if err != nil {
			logrus.Infof("Waiting to retrieve agent configuration; server is not ready: %v", err)
			for range ticker.C {
				continue RETRY
			}
		}
		return agentConfig
	}
}

// KubeProxyDisabled returns a bool indicating whether or not kube-proxy has been disabled in the
// server configuration. The server may not have a complete view of cluster configuration until
// after all startup hooks have completed, so a call to this will block until after the server's
// readyz endpoint returns OK.
func KubeProxyDisabled(ctx context.Context, node *config.Node, proxy proxy.Proxy) bool {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
RETRY:
	for {
		disabled, err := getKubeProxyDisabled(ctx, node, proxy)
		if err != nil {
			logrus.Infof("Waiting to retrieve kube-proxy configuration; server is not ready: %v", err)
			for range ticker.C {
				continue RETRY
			}
		}
		return disabled
	}
}

// APIServers returns a list of apiserver endpoints, suitable for seeding client loadbalancer configurations.
// This function will block until it can return a populated list of apiservers, or if the remote server returns
// an error (indicating that it does not support this functionality).
func APIServers(ctx context.Context, node *config.Node, proxy proxy.Proxy) []string {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
RETRY:
	for {
		addresses, err := getAPIServers(ctx, node, proxy)
		if err != nil {
			logrus.Infof("Failed to retrieve list of apiservers from server: %v", err)
			return nil
		}
		if len(addresses) == 0 {
			logrus.Infof("Waiting for apiserver addresses")
			for range ticker.C {
				continue RETRY
			}
		}
		return addresses
	}
}

type HTTPRequester func(u string, client *http.Client, username, password, token string) ([]byte, error)

func Request(path string, info *clientaccess.Info, requester HTTPRequester) ([]byte, error) {
	u, err := url.Parse(info.BaseURL)
	if err != nil {
		return nil, err
	}
	u.Path = path
	return requester(u.String(), clientaccess.GetHTTPClient(info.CACerts, info.CertFile, info.KeyFile), info.Username, info.Password, info.Token())
}

func getNodeNamedCrt(nodeName string, nodeIPs []net.IP, nodePasswordFile string) HTTPRequester {
	return func(u string, client *http.Client, username, password, token string) ([]byte, error) {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}

		if token != "" {
			req.Header.Add("Authorization", "Bearer "+token)
		} else if username != "" {
			req.SetBasicAuth(username, password)
		}

		req.Header.Set(version.Program+"-Node-Name", nodeName)
		nodePassword, err := ensureNodePassword(nodePasswordFile)
		if err != nil {
			return nil, err
		}
		req.Header.Set(version.Program+"-Node-Password", nodePassword)
		req.Header.Set(version.Program+"-Node-IP", util.JoinIPs(nodeIPs))

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		// If we got a 401 Unauthorized response when using client certs, try again without client cert auth.
		// This allows us to fall back from node identity to token when the node resource is deleted.
		if resp.StatusCode == http.StatusUnauthorized {
			if transport, ok := client.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil && len(transport.TLSClientConfig.Certificates) != 0 {
				logrus.Infof("Node authorization rejected, retrying without client certificate authentication")
				transport.TLSClientConfig.Certificates = []tls.Certificate{}
				resp, err = client.Do(req)
				if err != nil {
					return nil, err
				}
				defer resp.Body.Close()
			}
		}

		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("Node password rejected, duplicate hostname or contents of '%s' may not match server node-passwd entry, try enabling a unique node name with the --with-node-id flag", nodePasswordFile)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", u, resp.Status)
		}

		return io.ReadAll(resp.Body)
	}
}

func ensureNodeID(nodeIDFile string) (string, error) {
	if _, err := os.Stat(nodeIDFile); err == nil {
		id, err := os.ReadFile(nodeIDFile)
		return strings.TrimSpace(string(id)), err
	}
	id := make([]byte, 4, 4)
	_, err := cryptorand.Read(id)
	if err != nil {
		return "", err
	}
	nodeID := hex.EncodeToString(id)
	return nodeID, os.WriteFile(nodeIDFile, []byte(nodeID+"\n"), 0644)
}

func ensureNodePassword(nodePasswordFile string) (string, error) {
	if _, err := os.Stat(nodePasswordFile); err == nil {
		password, err := os.ReadFile(nodePasswordFile)
		return strings.TrimSpace(string(password)), err
	}
	password := make([]byte, 16, 16)
	_, err := cryptorand.Read(password)
	if err != nil {
		return "", err
	}
	nodePassword := hex.EncodeToString(password)
	return nodePassword, os.WriteFile(nodePasswordFile, []byte(nodePassword+"\n"), 0600)
}

func upgradeOldNodePasswordPath(oldNodePasswordFile, newNodePasswordFile string) {
	password, err := os.ReadFile(oldNodePasswordFile)
	if err != nil {
		return
	}
	if err := os.WriteFile(newNodePasswordFile, password, 0600); err != nil {
		logrus.Warnf("Unable to write password file: %v", err)
		return
	}
	if err := os.Remove(oldNodePasswordFile); err != nil {
		logrus.Warnf("Unable to remove old password file: %v", err)
		return
	}
}

func getServingCert(nodeName string, nodeIPs []net.IP, servingCertFile, servingKeyFile, nodePasswordFile string, info *clientaccess.Info) (*tls.Certificate, error) {
	servingCert, err := Request("/v1-"+version.Program+"/serving-kubelet.crt", info, getNodeNamedCrt(nodeName, nodeIPs, nodePasswordFile))
	if err != nil {
		return nil, err
	}

	servingCert, servingKey := splitCertKeyPEM(servingCert)

	if err := os.WriteFile(servingCertFile, servingCert, 0600); err != nil {
		return nil, errors.Wrapf(err, "failed to write node cert")
	}

	if err := os.WriteFile(servingKeyFile, servingKey, 0600); err != nil {
		return nil, errors.Wrapf(err, "failed to write node key")
	}

	cert, err := tls.X509KeyPair(servingCert, servingKey)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

func getHostFile(filename, keyFile string, info *clientaccess.Info) error {
	basename := filepath.Base(filename)
	fileBytes, err := info.Get("/v1-" + version.Program + "/" + basename)
	if err != nil {
		return err
	}
	if keyFile == "" {
		if err := os.WriteFile(filename, fileBytes, 0600); err != nil {
			return errors.Wrapf(err, "failed to write cert %s", filename)
		}
	} else {
		fileBytes, keyBytes := splitCertKeyPEM(fileBytes)
		if err := os.WriteFile(filename, fileBytes, 0600); err != nil {
			return errors.Wrapf(err, "failed to write cert %s", filename)
		}
		if err := os.WriteFile(keyFile, keyBytes, 0600); err != nil {
			return errors.Wrapf(err, "failed to write key %s", filename)
		}
	}
	return nil
}

func splitCertKeyPEM(bytes []byte) (certPem []byte, keyPem []byte) {
	for {
		b, rest := pem.Decode(bytes)
		if b == nil {
			break
		}
		bytes = rest

		if strings.Contains(b.Type, "PRIVATE KEY") {
			keyPem = append(keyPem, pem.EncodeToMemory(b)...)
		} else {
			certPem = append(certPem, pem.EncodeToMemory(b)...)
		}
	}

	return
}

func getNodeNamedHostFile(filename, keyFile, nodeName string, nodeIPs []net.IP, nodePasswordFile string, info *clientaccess.Info) error {
	basename := filepath.Base(filename)
	fileBytes, err := Request("/v1-"+version.Program+"/"+basename, info, getNodeNamedCrt(nodeName, nodeIPs, nodePasswordFile))
	if err != nil {
		return err
	}
	fileBytes, keyBytes := splitCertKeyPEM(fileBytes)

	if err := os.WriteFile(filename, fileBytes, 0600); err != nil {
		return errors.Wrapf(err, "failed to write cert %s", filename)
	}
	if err := os.WriteFile(keyFile, keyBytes, 0600); err != nil {
		return errors.Wrapf(err, "failed to write key %s", filename)
	}
	return nil
}

func isValidResolvConf(resolvConfFile string) bool {
	file, err := os.Open(resolvConfFile)
	if err != nil {
		return false
	}
	defer file.Close()

	nameserver := regexp.MustCompile(`^nameserver\s+([^\s]*)`)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		ipMatch := nameserver.FindStringSubmatch(scanner.Text())
		if len(ipMatch) == 2 {
			ip := net.ParseIP(ipMatch[1])
			if ip == nil || !ip.IsGlobalUnicast() {
				return false
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return false
	}
	return true
}

func locateOrGenerateResolvConf(envInfo *cmds.Agent) string {
	if envInfo.ResolvConf != "" {
		return envInfo.ResolvConf
	}
	resolvConfs := []string{"/etc/resolv.conf", "/run/systemd/resolve/resolv.conf"}
	for _, conf := range resolvConfs {
		if isValidResolvConf(conf) {
			return conf
		}
	}

	resolvConf := filepath.Join(envInfo.DataDir, "agent", "etc", "resolv.conf")
	if err := agentutil.WriteFile(resolvConf, "nameserver 8.8.8.8\n"); err != nil {
		logrus.Errorf("Failed to write %s: %v", resolvConf, err)
		return ""
	}
	logrus.Warnf("Host resolv.conf includes loopback or multicast nameservers - kubelet will use autogenerated resolv.conf with nameserver 8.8.8.8")
	return resolvConf
}

func get(ctx context.Context, envInfo *cmds.Agent, proxy proxy.Proxy) (*config.Node, error) {
	if envInfo.Debug {
		logrus.SetLevel(logrus.DebugLevel)
	}
	clientKubeletCert := filepath.Join(envInfo.DataDir, "agent", "client-kubelet.crt")
	clientKubeletKey := filepath.Join(envInfo.DataDir, "agent", "client-kubelet.key")
	withCert := clientaccess.WithClientCertificate(clientKubeletCert, clientKubeletKey)
	info, err := clientaccess.ParseAndValidateToken(proxy.SupervisorURL(), envInfo.Token, withCert)
	if err != nil {
		return nil, err
	}

	controlConfig, err := getConfig(info)
	if err != nil {
		return nil, errors.Wrap(err, "failed to retrieve configuration from server")
	}

	// If the supervisor and externally-facing apiserver are not on the same port, tell the proxy where to find the apiserver.
	if controlConfig.SupervisorPort != controlConfig.HTTPSPort {
		isIPv6 := utilsnet.IsIPv6(net.ParseIP([]string{envInfo.NodeIP.String()}[0]))
		if err := proxy.SetAPIServerPort(ctx, controlConfig.HTTPSPort, isIPv6); err != nil {
			return nil, errors.Wrapf(err, "failed to setup access to API Server port %d on at %s", controlConfig.HTTPSPort, proxy.SupervisorURL())
		}
	}

	var flannelIface *net.Interface
	if controlConfig.FlannelBackend != config.FlannelBackendNone && len(envInfo.FlannelIface) > 0 {
		flannelIface, err = net.InterfaceByName(envInfo.FlannelIface)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to find interface %s", envInfo.FlannelIface)
		}
	}

	clientCAFile := filepath.Join(envInfo.DataDir, "agent", "client-ca.crt")
	if err := getHostFile(clientCAFile, "", info); err != nil {
		return nil, err
	}

	serverCAFile := filepath.Join(envInfo.DataDir, "agent", "server-ca.crt")
	if err := getHostFile(serverCAFile, "", info); err != nil {
		return nil, err
	}

	servingKubeletCert := filepath.Join(envInfo.DataDir, "agent", "serving-kubelet.crt")
	servingKubeletKey := filepath.Join(envInfo.DataDir, "agent", "serving-kubelet.key")

	nodePasswordRoot := "/"
	if envInfo.Rootless {
		nodePasswordRoot = filepath.Join(envInfo.DataDir, "agent")
	}
	nodeConfigPath := filepath.Join(nodePasswordRoot, "etc", "rancher", "node")
	if err := os.MkdirAll(nodeConfigPath, 0755); err != nil {
		return nil, err
	}

	oldNodePasswordFile := filepath.Join(envInfo.DataDir, "agent", "node-password.txt")
	newNodePasswordFile := filepath.Join(nodeConfigPath, "password")
	upgradeOldNodePasswordPath(oldNodePasswordFile, newNodePasswordFile)

	nodeName, nodeIPs, err := util.GetHostnameAndIPs(envInfo.NodeName, envInfo.NodeIP)
	if err != nil {
		return nil, err
	}

	// If there is a VPN, we must overwrite NodeIP and flannel interface
	var vpnInfo vpn.VPNInfo
	if envInfo.VPNAuth != "" {
		vpnInfo, err = vpn.GetVPNInfo(envInfo.VPNAuth)
		if err != nil {
			return nil, err
		}

		// Pass ipv4, ipv6 or both depending on nodeIPs mode
		var vpnIPs []net.IP
		if utilsnet.IsIPv4(nodeIPs[0]) && vpnInfo.IPv4Address != nil {
			vpnIPs = append(vpnIPs, vpnInfo.IPv4Address)
			if vpnInfo.IPv6Address != nil {
				vpnIPs = append(vpnIPs, vpnInfo.IPv6Address)
			}
		} else if utilsnet.IsIPv6(nodeIPs[0]) && vpnInfo.IPv6Address != nil {
			vpnIPs = append(vpnIPs, vpnInfo.IPv6Address)
			if vpnInfo.IPv4Address != nil {
				vpnIPs = append(vpnIPs, vpnInfo.IPv4Address)
			}
		} else {
			return nil, errors.Errorf("address family mismatch when assigning VPN addresses to node: node=%v, VPN ipv4=%v ipv6=%v", nodeIPs, vpnInfo.IPv4Address, vpnInfo.IPv6Address)
		}

		// Overwrite nodeip and flannel interface and throw a warning if user explicitly set those parameters
		if len(vpnIPs) != 0 {
			logrus.Infof("Node-ip changed to %v due to VPN", vpnIPs)
			if len(envInfo.NodeIP) != 0 {
				logrus.Warn("VPN provider overrides configured node-ip parameter")
			}
			if len(envInfo.NodeExternalIP) != 0 {
				logrus.Warn("VPN provider overrides node-external-ip parameter")
			}
			nodeIPs = vpnIPs
			flannelIface, err = net.InterfaceByName(vpnInfo.VPNInterface)
			if err != nil {
				return nil, errors.Wrapf(err, "unable to find vpn interface: %s", vpnInfo.VPNInterface)
			}
		}
	}

	nodeExternalIPs, err := util.ParseStringSliceToIPs(envInfo.NodeExternalIP)
	if err != nil {
		return nil, fmt.Errorf("invalid node-external-ip: %w", err)
	}

	if envInfo.WithNodeID {
		nodeID, err := ensureNodeID(filepath.Join(nodeConfigPath, "id"))
		if err != nil {
			return nil, err
		}
		nodeName += "-" + nodeID
	}

	os.Setenv("NODE_NAME", nodeName)

	nodeExternalAndInternalIPs := append(nodeIPs, nodeExternalIPs...)
	servingCert, err := getServingCert(nodeName, nodeExternalAndInternalIPs, servingKubeletCert, servingKubeletKey, newNodePasswordFile, info)
	if err != nil {
		return nil, err
	}

	if err := getNodeNamedHostFile(clientKubeletCert, clientKubeletKey, nodeName, nodeIPs, newNodePasswordFile, info); err != nil {
		return nil, err
	}

	kubeconfigKubelet := filepath.Join(envInfo.DataDir, "agent", "kubelet.kubeconfig")
	if err := deps.KubeConfig(kubeconfigKubelet, proxy.APIServerURL(), serverCAFile, clientKubeletCert, clientKubeletKey); err != nil {
		return nil, err
	}

	clientKubeProxyCert := filepath.Join(envInfo.DataDir, "agent", "client-kube-proxy.crt")
	clientKubeProxyKey := filepath.Join(envInfo.DataDir, "agent", "client-kube-proxy.key")
	if err := getHostFile(clientKubeProxyCert, clientKubeProxyKey, info); err != nil {
		return nil, err
	}

	kubeconfigKubeproxy := filepath.Join(envInfo.DataDir, "agent", "kubeproxy.kubeconfig")
	if err := deps.KubeConfig(kubeconfigKubeproxy, proxy.APIServerURL(), serverCAFile, clientKubeProxyCert, clientKubeProxyKey); err != nil {
		return nil, err
	}

	clientK3sControllerCert := filepath.Join(envInfo.DataDir, "agent", "client-"+version.Program+"-controller.crt")
	clientK3sControllerKey := filepath.Join(envInfo.DataDir, "agent", "client-"+version.Program+"-controller.key")
	if err := getHostFile(clientK3sControllerCert, clientK3sControllerKey, info); err != nil {
		return nil, err
	}

	kubeconfigK3sController := filepath.Join(envInfo.DataDir, "agent", version.Program+"controller.kubeconfig")
	if err := deps.KubeConfig(kubeconfigK3sController, proxy.APIServerURL(), serverCAFile, clientK3sControllerCert, clientK3sControllerKey); err != nil {
		return nil, err
	}

	nodeConfig := &config.Node{
		Docker:                   envInfo.Docker,
		SELinux:                  envInfo.EnableSELinux,
		ContainerRuntimeEndpoint: envInfo.ContainerRuntimeEndpoint,
		ImageServiceEndpoint:     envInfo.ImageServiceEndpoint,
		MultiClusterCIDR:         controlConfig.MultiClusterCIDR,
		FlannelBackend:           controlConfig.FlannelBackend,
		FlannelIPv6Masq:          controlConfig.FlannelIPv6Masq,
		FlannelExternalIP:        controlConfig.FlannelExternalIP,
		EgressSelectorMode:       controlConfig.EgressSelectorMode,
		ServerHTTPSPort:          controlConfig.HTTPSPort,
		Token:                    info.String(),
	}
	nodeConfig.FlannelIface = flannelIface
	nodeConfig.Images = filepath.Join(envInfo.DataDir, "agent", "images")
	nodeConfig.AgentConfig.NodeName = nodeName
	nodeConfig.AgentConfig.NodeConfigPath = nodeConfigPath
	nodeConfig.AgentConfig.ClientKubeletCert = clientKubeletCert
	nodeConfig.AgentConfig.ClientKubeletKey = clientKubeletKey
	nodeConfig.AgentConfig.ServingKubeletCert = servingKubeletCert
	nodeConfig.AgentConfig.ServingKubeletKey = servingKubeletKey
	nodeConfig.AgentConfig.ClusterDNS = controlConfig.ClusterDNS
	nodeConfig.AgentConfig.ClusterDomain = controlConfig.ClusterDomain
	nodeConfig.AgentConfig.ResolvConf = locateOrGenerateResolvConf(envInfo)
	nodeConfig.AgentConfig.ClientCA = clientCAFile
	nodeConfig.AgentConfig.KubeConfigKubelet = kubeconfigKubelet
	nodeConfig.AgentConfig.KubeConfigKubeProxy = kubeconfigKubeproxy
	nodeConfig.AgentConfig.KubeConfigK3sController = kubeconfigK3sController
	if envInfo.Rootless {
		nodeConfig.AgentConfig.RootDir = filepath.Join(envInfo.DataDir, "agent", "kubelet")
	}
	nodeConfig.AgentConfig.Snapshotter = envInfo.Snapshotter
	nodeConfig.AgentConfig.IPSECPSK = controlConfig.IPSECPSK
	nodeConfig.Containerd.Config = filepath.Join(envInfo.DataDir, "agent", "etc", "containerd", "config.toml")
	nodeConfig.Containerd.Root = filepath.Join(envInfo.DataDir, "agent", "containerd")
	nodeConfig.CRIDockerd.Root = filepath.Join(envInfo.DataDir, "agent", "cri-dockerd")
	if !nodeConfig.Docker {
		if nodeConfig.ImageServiceEndpoint != "" {
			nodeConfig.AgentConfig.ImageServiceSocket = nodeConfig.ImageServiceEndpoint
		} else if nodeConfig.ContainerRuntimeEndpoint == "" {
			switch nodeConfig.AgentConfig.Snapshotter {
			case "overlayfs":
				if err := containerd.OverlaySupported(nodeConfig.Containerd.Root); err != nil {
					return nil, errors.Wrapf(err, "\"overlayfs\" snapshotter cannot be enabled for %q, try using \"fuse-overlayfs\" or \"native\"",
						nodeConfig.Containerd.Root)
				}
			case "fuse-overlayfs":
				if err := containerd.FuseoverlayfsSupported(nodeConfig.Containerd.Root); err != nil {
					return nil, errors.Wrapf(err, "\"fuse-overlayfs\" snapshotter cannot be enabled for %q, try using \"native\"",
						nodeConfig.Containerd.Root)
				}
			case "stargz":
				if err := containerd.StargzSupported(nodeConfig.Containerd.Root); err != nil {
					return nil, errors.Wrapf(err, "\"stargz\" snapshotter cannot be enabled for %q, try using \"overlayfs\" or \"native\"",
						nodeConfig.Containerd.Root)
				}
				nodeConfig.AgentConfig.ImageServiceSocket = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
			}
		} else {
			nodeConfig.AgentConfig.ImageServiceSocket = nodeConfig.ContainerRuntimeEndpoint
		}
	}
	nodeConfig.Containerd.Opt = filepath.Join(envInfo.DataDir, "agent", "containerd")
	nodeConfig.Containerd.Log = filepath.Join(envInfo.DataDir, "agent", "containerd", "containerd.log")
	nodeConfig.Containerd.Debug = envInfo.Debug
	applyContainerdStateAndAddress(nodeConfig)
	applyCRIDockerdAddress(nodeConfig)
	nodeConfig.Containerd.Template = filepath.Join(envInfo.DataDir, "agent", "etc", "containerd", "config.toml.tmpl")
	nodeConfig.Certificate = servingCert

	nodeConfig.AgentConfig.NodeIPs = nodeIPs
	listenAddress, _, _, err := util.GetDefaultAddresses(nodeIPs[0])
	if err != nil {
		return nil, errors.Wrap(err, "cannot configure IPv4/IPv6 node-ip")
	}
	nodeConfig.AgentConfig.NodeIP = nodeIPs[0].String()
	nodeConfig.AgentConfig.ListenAddress = listenAddress
	nodeConfig.AgentConfig.NodeExternalIPs = nodeExternalIPs

	// if configured, set NodeExternalIP to the first IPv4 address, for legacy clients
	// unless only IPv6 address given
	if len(nodeConfig.AgentConfig.NodeExternalIPs) > 0 {
		nodeConfig.AgentConfig.NodeExternalIP = nodeConfig.AgentConfig.NodeExternalIPs[0].String()
	}

	nodeConfig.NoFlannel = nodeConfig.FlannelBackend == config.FlannelBackendNone
	if !nodeConfig.NoFlannel {
		hostLocal, err := exec.LookPath("host-local")
		if err != nil {
			return nil, errors.Wrapf(err, "failed to find host-local")
		}

		if envInfo.FlannelConf == "" {
			nodeConfig.FlannelConfFile = filepath.Join(envInfo.DataDir, "agent", "etc", "flannel", "net-conf.json")
		} else {
			nodeConfig.FlannelConfFile = envInfo.FlannelConf
			nodeConfig.FlannelConfOverride = true
		}
		nodeConfig.AgentConfig.CNIBinDir = filepath.Dir(hostLocal)
		nodeConfig.AgentConfig.CNIConfDir = filepath.Join(envInfo.DataDir, "agent", "etc", "cni", "net.d")
		nodeConfig.AgentConfig.FlannelCniConfFile = envInfo.FlannelCniConfFile

		// It does not make sense to use VPN without its flannel backend
		if envInfo.VPNAuth != "" {
			nodeConfig.FlannelBackend = vpnInfo.ProviderName
		}
	}

	if nodeConfig.Docker {
		nodeConfig.AgentConfig.CNIPlugin = true
		nodeConfig.AgentConfig.RuntimeSocket = nodeConfig.CRIDockerd.Address
	} else if nodeConfig.ContainerRuntimeEndpoint == "" {
		nodeConfig.AgentConfig.RuntimeSocket = nodeConfig.Containerd.Address
	} else {
		nodeConfig.AgentConfig.RuntimeSocket = nodeConfig.ContainerRuntimeEndpoint
	}

	if controlConfig.ClusterIPRange != nil {
		nodeConfig.AgentConfig.ClusterCIDR = controlConfig.ClusterIPRange
		nodeConfig.AgentConfig.ClusterCIDRs = []*net.IPNet{controlConfig.ClusterIPRange}
	}

	if len(controlConfig.ClusterIPRanges) > 0 {
		nodeConfig.AgentConfig.ClusterCIDRs = controlConfig.ClusterIPRanges
	}

	if controlConfig.ServiceIPRange != nil {
		nodeConfig.AgentConfig.ServiceCIDR = controlConfig.ServiceIPRange
		nodeConfig.AgentConfig.ServiceCIDRs = []*net.IPNet{controlConfig.ServiceIPRange}
	}

	if len(controlConfig.ServiceIPRanges) > 0 {
		nodeConfig.AgentConfig.ServiceCIDRs = controlConfig.ServiceIPRanges
	}

	if controlConfig.ServiceNodePortRange != nil {
		nodeConfig.AgentConfig.ServiceNodePortRange = *controlConfig.ServiceNodePortRange
	}

	if len(controlConfig.ClusterDNSs) == 0 {
		nodeConfig.AgentConfig.ClusterDNSs = []net.IP{controlConfig.ClusterDNS}
	} else {
		nodeConfig.AgentConfig.ClusterDNSs = controlConfig.ClusterDNSs
	}

	nodeConfig.AgentConfig.PauseImage = envInfo.PauseImage
	nodeConfig.AgentConfig.AirgapExtraRegistry = envInfo.AirgapExtraRegistry
	nodeConfig.AgentConfig.SystemDefaultRegistry = controlConfig.SystemDefaultRegistry

	// Apply SystemDefaultRegistry to PauseImage and AirgapExtraRegistry
	if controlConfig.SystemDefaultRegistry != "" {
		if nodeConfig.AgentConfig.PauseImage != "" && !strings.HasPrefix(nodeConfig.AgentConfig.PauseImage, controlConfig.SystemDefaultRegistry) {
			nodeConfig.AgentConfig.PauseImage = controlConfig.SystemDefaultRegistry + "/" + nodeConfig.AgentConfig.PauseImage
		}
		if !slice.ContainsString(nodeConfig.AgentConfig.AirgapExtraRegistry, controlConfig.SystemDefaultRegistry) {
			nodeConfig.AgentConfig.AirgapExtraRegistry = append(nodeConfig.AgentConfig.AirgapExtraRegistry, controlConfig.SystemDefaultRegistry)
		}
	}

	nodeConfig.AgentConfig.ExtraKubeletArgs = envInfo.ExtraKubeletArgs
	nodeConfig.AgentConfig.ExtraKubeProxyArgs = envInfo.ExtraKubeProxyArgs
	nodeConfig.AgentConfig.NodeTaints = envInfo.Taints
	nodeConfig.AgentConfig.NodeLabels = envInfo.Labels
	nodeConfig.AgentConfig.ImageCredProvBinDir = envInfo.ImageCredProvBinDir
	nodeConfig.AgentConfig.ImageCredProvConfig = envInfo.ImageCredProvConfig
	nodeConfig.AgentConfig.PrivateRegistry = envInfo.PrivateRegistry
	nodeConfig.AgentConfig.DisableCCM = controlConfig.DisableCCM
	nodeConfig.AgentConfig.DisableNPC = controlConfig.DisableNPC
	nodeConfig.AgentConfig.Rootless = envInfo.Rootless
	nodeConfig.AgentConfig.PodManifests = filepath.Join(envInfo.DataDir, "agent", DefaultPodManifestPath)
	nodeConfig.AgentConfig.ProtectKernelDefaults = envInfo.ProtectKernelDefaults
	nodeConfig.AgentConfig.DisableServiceLB = envInfo.DisableServiceLB

	if err := validateNetworkConfig(nodeConfig); err != nil {
		return nil, err
	}

	return nodeConfig, nil
}

// getAPIServers attempts to return a list of apiservers from the server.
func getAPIServers(ctx context.Context, node *config.Node, proxy proxy.Proxy) ([]string, error) {
	withCert := clientaccess.WithClientCertificate(node.AgentConfig.ClientKubeletCert, node.AgentConfig.ClientKubeletKey)
	info, err := clientaccess.ParseAndValidateToken(proxy.SupervisorURL(), node.Token, withCert)
	if err != nil {
		return nil, err
	}

	data, err := info.Get("/v1-" + version.Program + "/apiservers")
	if err != nil {
		return nil, err
	}

	endpoints := []string{}
	return endpoints, json.Unmarshal(data, &endpoints)
}

// getKubeProxyDisabled attempts to return the DisableKubeProxy setting from the server configuration data.
// It first checks the server readyz endpoint, to ensure that the configuration has stabilized before use.
func getKubeProxyDisabled(ctx context.Context, node *config.Node, proxy proxy.Proxy) (bool, error) {
	withCert := clientaccess.WithClientCertificate(node.AgentConfig.ClientKubeletCert, node.AgentConfig.ClientKubeletKey)
	info, err := clientaccess.ParseAndValidateToken(proxy.SupervisorURL(), node.Token, withCert)
	if err != nil {
		return false, err
	}

	// 500 error indicates that the health check has failed; other errors (for example 401 Unauthorized)
	// indicate that the server is down-level and doesn't support readyz, so we should just use whatever
	// the server has for us.
	if err := getReadyz(info); err != nil && strings.HasSuffix(err.Error(), "500 Internal Server Error") {
		return false, err
	}

	controlConfig, err := getConfig(info)
	if err != nil {
		return false, errors.Wrap(err, "failed to retrieve configuration from server")
	}

	return controlConfig.DisableKubeProxy, nil
}

// getConfig returns server configuration data. Note that this may be mutated during system startup; anything that needs
// to ensure stable system state should check the readyz endpoint first. This is required because RKE2 starts up the
// kubelet early, before the apiserver is available.
func getConfig(info *clientaccess.Info) (*config.Control, error) {
	data, err := info.Get("/v1-" + version.Program + "/config")
	if err != nil {
		return nil, err
	}

	controlControl := &config.Control{}
	return controlControl, json.Unmarshal(data, controlControl)
}

// getReadyz returns nil if the server is ready, or an error if not.
func getReadyz(info *clientaccess.Info) error {
	_, err := info.Get("/v1-" + version.Program + "/readyz")
	return err
}

// validateNetworkConfig ensures that the network configuration values provided by the server make sense.
func validateNetworkConfig(nodeConfig *config.Node) error {
	// Old versions of the server do not send enough information to correctly start the NPC. Users
	// need to upgrade the server to at least the same version as the agent, or disable the NPC
	// cluster-wide.
	if nodeConfig.AgentConfig.DisableNPC == false && (nodeConfig.AgentConfig.ServiceCIDR == nil || nodeConfig.AgentConfig.ServiceNodePortRange.Size == 0) {
		return fmt.Errorf("incompatible down-level server detected; servers must be upgraded to at least %s, or restarted with --disable-network-policy", version.Version)
	}

	return nil
}
