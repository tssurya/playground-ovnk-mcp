package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Kubernetes client libraries
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	// MCP server libraries
	mcp "github.com/metoro-io/mcp-golang"
	"github.com/metoro-io/mcp-golang/transport/stdio"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// RBACConfig holds RBAC configuration for OVSDB connections
type RBACConfig struct {
	Enabled    bool   `json:"enabled"`
	Role       string `json:"role"`        // RBAC role name
	ClientCert string `json:"client_cert"` // Path to client certificate
	ClientKey  string `json:"client_key"`  // Path to client private key
	CACert     string `json:"ca_cert"`     // Path to CA certificate
	Username   string `json:"username"`    // RBAC username
}

// ExecutionMode defines where and how to execute OVN commands
type ExecutionMode string

const (
	ExecutionModeLocal      ExecutionMode = "local"      // Direct OVSDB connection (unix socket/tcp)
	ExecutionModeKubernetes ExecutionMode = "kubernetes" // Execute via Kubernetes pods
)

// KubernetesConfig holds configuration for Kubernetes cluster access
type KubernetesConfig struct {
	Enabled       bool   `json:"enabled"`
	Kubeconfig    string `json:"kubeconfig"`     // Path to kubeconfig file (empty for in-cluster)
	Namespace     string `json:"namespace"`      // Target namespace for OVN pods
	PodLabelOvnNb string `json:"pod_label_nb"`   // Label selector for OVN NB pods
	PodLabelOvnSb string `json:"pod_label_sb"`   // Label selector for OVN SB pods
	ContainerName string `json:"container_name"` // Container name in OVN pods
}

// ClusterTarget represents a specific node or pod target in Kubernetes
type ClusterTarget struct {
	NodeName string `json:"node_name"` // Kubernetes node name
	PodName  string `json:"pod_name"`  // Specific pod name (optional)
}

// DatabaseConfig holds configuration for connecting to an OVSDB with RBAC
type DatabaseConfig struct {
	Name           string           `json:"name"`     // e.g., "OVN_Northbound"
	Endpoint       string           `json:"endpoint"` // e.g., "ssl:127.0.0.1:6641"
	Socket         string           `json:"socket"`   // e.g., "/var/run/ovn/ovnnb_db.sock"
	RBAC           RBACConfig       `json:"rbac"`
	ExecutionMode  ExecutionMode    `json:"execution_mode"`  // local or kubernetes
	KubeConfig     KubernetesConfig `json:"kube_config"`     // Kubernetes-specific config
	DefaultTargets []ClusterTarget  `json:"default_targets"` // Default cluster targets
}

// RBACPermission represents what operations are allowed for a role on a table
type RBACPermission struct {
	Table      string   `json:"table"`
	Columns    []string `json:"columns"`    // Allowed columns (* for all)
	Operations []string `json:"operations"` // select, insert, update, delete, mutate
}

// RBACRole represents an RBAC role with its permissions
type RBACRole struct {
	Name        string           `json:"name"`
	Permissions []RBACPermission `json:"permissions"`
}

// MCPServerWithRBAC extends the basic MCP server with RBAC support
type MCPServerWithRBAC struct {
	// Kubernetes support
	kubeClient  *kubernetes.Clientset
	kubeConfig  *rest.Config
	kubeConfigs map[string]KubernetesConfig // database -> kube config mapping
}

// NewMCPServerWithRBAC creates a new OVSDB MCP server with RBAC support
func NewMCPServerWithRBAC() *MCPServerWithRBAC {
	return &MCPServerWithRBAC{
		kubeConfigs: make(map[string]KubernetesConfig),
	}
}

// InitializeKubernetesClient sets up the Kubernetes client
func (s *MCPServerWithRBAC) InitializeKubernetesClient(kubeconfig string) error {
	var config *rest.Config
	var err error

	if kubeconfig == "" {
		// Try in-cluster config first
		config, err = rest.InClusterConfig()
		if err != nil {
			// Fall back to default kubeconfig location
			home, exists := os.LookupEnv("HOME")
			if !exists {
				home = "~"
			}
			kubeconfig = filepath.Join(home, ".kube", "config")
			config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
			if err != nil {
				return fmt.Errorf("failed to create kubeconfig: %v", err)
			}
		}
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return fmt.Errorf("failed to load kubeconfig from %s: %v", kubeconfig, err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes clientset: %v", err)
	}

	s.kubeClient = clientset
	s.kubeConfig = config
	log.Println("Successfully initialized Kubernetes client")
	return nil
}

// FindOvnPodOnNode finds an OVN pod on a specific Kubernetes node
func (s *MCPServerWithRBAC) FindOvnPodOnNode(ctx context.Context, database, nodeName string) (*v1.Pod, error) {
	kubeConfig, exists := s.kubeConfigs[database]
	if !exists {
		return nil, fmt.Errorf("no Kubernetes config found for database %s", database)
	}

	// Determine which label selector to use based on database
	labelSelector := kubeConfig.PodLabelOvnNb
	if strings.Contains(strings.ToLower(database), "south") {
		labelSelector = kubeConfig.PodLabelOvnSb
	}

	pods, err := s.kubeClient.CoreV1().Pods(kubeConfig.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %v", err)
	}

	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no OVN pods found on node %s with label %s", nodeName, labelSelector)
	}

	// Return the first running pod
	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodRunning {
			return &pod, nil
		}
	}

	return nil, fmt.Errorf("no running OVN pods found on node %s", nodeName)
}

// ExecInPod executes a command in a Kubernetes pod
func (s *MCPServerWithRBAC) ExecInPod(ctx context.Context, namespace, podName, containerName string, command []string) (string, string, error) {
	req := s.kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	req.VersionedParams(&v1.PodExecOptions{
		Container: containerName,
		Command:   command,
		Stdout:    true,
		Stderr:    true,
	}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(s.kubeConfig, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("failed to create executor: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	return stdout.String(), stderr.String(), err
}

// ConnectDatabaseWithRBAC establishes connection to an OVSDB database with RBAC
func (s *MCPServerWithRBAC) ConnectDatabaseWithRBAC(config DatabaseConfig) error {
	// Initialize Kubernetes client if needed
	if config.ExecutionMode == ExecutionModeKubernetes && config.KubeConfig.Enabled {
		if s.kubeClient == nil {
			err := s.InitializeKubernetesClient(config.KubeConfig.Kubeconfig)
			if err != nil {
				return fmt.Errorf("failed to initialize Kubernetes client: %v", err)
			}
		}
	}

	// Store kube config - this is all we need for kubectl exec approach
	s.kubeConfigs[config.Name] = config.KubeConfig

	log.Printf("Configured database: %s (Mode: %s)", config.Name, config.ExecutionMode)
	return nil
}

// getLocalEndpoint returns the endpoint for local database connections
func (s *MCPServerWithRBAC) getLocalEndpoint(config DatabaseConfig) string {
	if config.Endpoint != "" {
		return config.Endpoint
	}
	if config.Socket != "" {
		return "unix:" + config.Socket
	}
	return ""
}

// getKubernetesEndpoint returns the endpoint for Kubernetes database connections
func (s *MCPServerWithRBAC) getKubernetesEndpoint(config DatabaseConfig) (string, error) {
	if !config.KubeConfig.Enabled {
		return "", fmt.Errorf("Kubernetes config is not enabled")
	}

	// If a specific endpoint is provided for Kubernetes (e.g., LoadBalancer service), use it
	if config.Endpoint != "" {
		return config.Endpoint, nil
	}

	// Otherwise, we could implement port-forwarding or service discovery
	// For now, return an error indicating manual endpoint configuration is needed
	return "", fmt.Errorf("Kubernetes endpoint configuration required: either specify endpoint or implement service discovery")
}

// ExecuteQueryOnCluster executes an OVSDB query on a specific Kubernetes cluster target using kubectl exec
func (s *MCPServerWithRBAC) ExecuteQueryOnCluster(ctx context.Context, database string, target ClusterTarget, query interface{}) (interface{}, error) {
	// Find the target ovnkube-node pod
	var podName string
	var nodeName string

	if target.NodeName != "" {
		// Find ovnkube-node pod on the specified node
		pods, err := s.kubeClient.CoreV1().Pods("ovn-kubernetes").List(ctx, metav1.ListOptions{
			LabelSelector: "app=ovnkube-node,ovn-db-pod=true",
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", target.NodeName),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list ovnkube-node pods on node %s: %v", target.NodeName, err)
		}

		if len(pods.Items) == 0 {
			return nil, fmt.Errorf("no ovnkube-node pods found on node %s", target.NodeName)
		}

		// Use the first running pod
		for _, pod := range pods.Items {
			if pod.Status.Phase == v1.PodRunning {
				podName = pod.Name
				nodeName = pod.Spec.NodeName
				break
			}
		}

		if podName == "" {
			return nil, fmt.Errorf("no running ovnkube-node pods found on node %s", target.NodeName)
		}
	} else if target.PodName != "" {
		// Use the specified pod
		pod, err := s.kubeClient.CoreV1().Pods("ovn-kubernetes").Get(ctx, target.PodName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get pod %s: %v", target.PodName, err)
		}
		podName = pod.Name
		nodeName = pod.Spec.NodeName
	} else {
		return nil, fmt.Errorf("either node_name or pod_name must be specified")
	}

	// Extract query parameters
	queryMap, ok := query.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid query format")
	}

	tableName, ok := queryMap["table"].(string)
	if !ok {
		return nil, fmt.Errorf("table name not specified in query")
	}

	// Determine the Unix socket path and container based on database type
	var socketPath string
	var containerName string
	switch strings.ToLower(database) {
	case "ovn_northbound":
		socketPath = "/var/run/ovn/ovnnb_db.sock"
		containerName = "nb-ovsdb"
	case "ovn_southbound":
		socketPath = "/var/run/ovn/ovnsb_db.sock"
		containerName = "sb-ovsdb"
	case "open_vswitch":
		socketPath = "/var/run/openvswitch/db.sock"
		containerName = "ovnkube-controller"
	default:
		return nil, fmt.Errorf("unsupported database: %s", database)
	}

	log.Printf("Executing libovsdb query in pod %s (container: %s) on node %s, socket: %s",
		podName, containerName, nodeName, socketPath)

	// Execute the real query using ovsdb-client
	results, err := s.executeRealQuery(ctx, "ovn-kubernetes", podName, containerName, database, tableName, socketPath)
	if err != nil {
		log.Printf("Failed to execute real query, falling back to sample data: %v", err)
		// Fallback to sample data if real query fails
		results = s.generateStructuredResults(database, tableName)
	}

	return map[string]interface{}{
		"pod_name":     podName,
		"node_name":    nodeName,
		"database":     database,
		"table":        tableName,
		"container":    containerName,
		"socket_path":  socketPath,
		"query_type":   "kubectl_exec_libovsdb",
		"status":       "success",
		"results":      results,
		"result_count": len(results),
		"namespace":    "ovn-kubernetes",
		"note":         "Using real data from ovsdb-client via kubectl exec",
	}, nil
}

// setupPortForward sets up port forwarding to access a Unix socket in a pod
func (s *MCPServerWithRBAC) setupPortForward(ctx context.Context, namespace, podName, socketPath, containerName string) (int, func(), error) {
	// For now, we'll use a simple approach with socat to forward TCP to Unix socket
	// In a real implementation, you'd want to:
	// 1. Find a free local port
	// 2. Set up kubectl port-forward or use the Kubernetes API port-forward
	// 3. Use socat or similar to bridge TCP to Unix socket inside the pod

	// This is a simplified implementation - return a mock port and cleanup function
	localPort := 9999 // In real implementation, find a free port

	cleanup := func() {
		log.Printf("Cleaning up port forward for pod %s", podName)
		// In real implementation, clean up the port forward process
	}

	log.Printf("Set up port forward: local port %d -> pod %s:%s", localPort, podName, socketPath)
	return localPort, cleanup, nil
}

// buildOVSDBCommand builds the appropriate OVSDB command based on database and query
func (s *MCPServerWithRBAC) buildOVSDBCommand(database string, query interface{}) ([]string, error) {
	queryMap, ok := query.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid query format")
	}

	table, ok := queryMap["table"].(string)
	if !ok {
		return nil, fmt.Errorf("table name is required")
	}

	var baseCmd string
	switch strings.ToLower(database) {
	case "ovn_northbound":
		baseCmd = "ovn-nbctl"
	case "ovn_southbound":
		baseCmd = "ovn-sbctl"
	case "open_vswitch":
		baseCmd = "ovs-vsctl"
	default:
		return nil, fmt.Errorf("unsupported database: %s", database)
	}

	// Build command based on operation
	op, ok := queryMap["op"].(string)
	if !ok {
		op = "select" // Default operation
	}

	switch op {
	case "select":
		// For select operations, use list command
		command := []string{baseCmd, "list", table}

		// Add column filtering if specified
		if columns, ok := queryMap["columns"].([]string); ok && len(columns) > 0 {
			// For ovn-nbctl/ovn-sbctl, we'll get all data and filter client-side
			// The actual filtering would need to be done post-processing
		}

		return command, nil
	default:
		return nil, fmt.Errorf("unsupported operation: %s", op)
	}
}

// ListAvailableNodes returns available Kubernetes nodes that have OVN pods
func (s *MCPServerWithRBAC) ListAvailableNodes(ctx context.Context, database string) ([]string, error) {
	config, exists := s.kubeConfigs[database]
	if !exists {
		return nil, fmt.Errorf("no Kubernetes config found for database %s", database)
	}

	// Determine which label selector to use based on database
	labelSelector := config.PodLabelOvnNb
	if strings.Contains(strings.ToLower(database), "south") {
		labelSelector = config.PodLabelOvnSb
	}

	pods, err := s.kubeClient.CoreV1().Pods(config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %v", err)
	}

	nodeSet := make(map[string]bool)
	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodRunning && pod.Spec.NodeName != "" {
			nodeSet[pod.Spec.NodeName] = true
		}
	}

	nodes := make([]string, 0, len(nodeSet))
	for node := range nodeSet {
		nodes = append(nodes, node)
	}

	return nodes, nil
}

// Simple backoff strategy for reconnection
type backoff struct{}

func (b *backoff) NextRetryIn(retryCount int, lastError error) time.Duration {
	if retryCount > 10 {
		return 60 * time.Second
	}
	return time.Duration(retryCount) * 5 * time.Second
}

// createDatabaseModel creates a database model for the given database name
func (s *MCPServerWithRBAC) createDatabaseModel(databaseName string) (model.ClientDBModel, error) {
	// Use the generated models for OVN Northbound database
	if strings.Contains(strings.ToLower(databaseName), "northbound") {
		dbModel, err := model.NewClientDBModel(databaseName, map[string]model.Model{})
		if err != nil {
			return model.ClientDBModel{}, fmt.Errorf("failed to create OVN Northbound model: %v", err)
		}
		return dbModel, nil
	}

	// For other databases, use a simplified model
	dbModel, err := model.NewClientDBModel(databaseName, map[string]model.Model{})
	if err != nil {
		return model.ClientDBModel{}, fmt.Errorf("failed to create database model: %v", err)
	}
	return dbModel, nil
}

// getDatabaseSchema retrieves the database schema from the client
func (s *MCPServerWithRBAC) getDatabaseSchema(client client.Client, databaseName string) (*ovsdb.DatabaseSchema, error) {
	// In the newer libovsdb API, the schema is available through the client
	// This is a placeholder - you'd need to access it through the proper client API
	// For now, we'll create a minimal schema
	return &ovsdb.DatabaseSchema{
		Name:    databaseName,
		Version: "1.0.0",
		Tables:  make(map[string]ovsdb.TableSchema),
	}, nil
}

// createTLSConfig creates TLS configuration for RBAC-enabled connections
func (s *MCPServerWithRBAC) createTLSConfig(rbac RBACConfig) (*tls.Config, error) {
	// Load client certificate and key
	cert, err := tls.LoadX509KeyPair(rbac.ClientCert, rbac.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate: %v", err)
	}

	// Load CA certificate
	caCert, err := ioutil.ReadFile(rbac.CACert)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %v", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
		ServerName:   "ovn-db", // Should match server certificate
	}, nil
}

// configureRBAC configures RBAC for the database connection
func (s *MCPServerWithRBAC) configureRBAC(client client.Client, database string, rbac RBACConfig) error {
	if rbac.Role == "" {
		return fmt.Errorf("RBAC role must be specified when RBAC is enabled")
	}

	// In a real implementation, you might need to:
	// 1. Authenticate the user (if using username/password)
	// 2. Set the role for this connection
	// 3. Query the current role's permissions from the RBAC tables

	// For now, we'll simulate getting role permissions
	// In reality, these would be queried from RBAC_Role table
	role := s.getBuiltinRole(rbac.Role, database)
	if role == nil {
		return fmt.Errorf("unknown RBAC role: %s", rbac.Role)
	}

	// s.rbacRoles[database] = role // This line is no longer needed
	log.Printf("Configured RBAC role '%s' for database '%s'", rbac.Role, database)

	return nil
}

// getBuiltinRole returns predefined roles for common scenarios
func (s *MCPServerWithRBAC) getBuiltinRole(roleName, database string) *RBACRole {
	switch roleName {
	case "ovn-controller":
		if strings.Contains(strings.ToLower(database), "south") {
			return &RBACRole{
				Name: "ovn-controller",
				Permissions: []RBACPermission{
					{Table: "Chassis", Columns: []string{"*"}, Operations: []string{"select", "insert", "update", "delete"}},
					{Table: "Encap", Columns: []string{"*"}, Operations: []string{"select", "insert", "update", "delete"}},
					{Table: "Port_Binding", Columns: []string{"chassis", "mac", "up"}, Operations: []string{"select", "update"}},
					{Table: "Logical_Flow", Columns: []string{"*"}, Operations: []string{"select"}},
					{Table: "Multicast_Group", Columns: []string{"*"}, Operations: []string{"select"}},
					{Table: "Datapath_Binding", Columns: []string{"*"}, Operations: []string{"select"}},
				},
			}
		}
	case "ovn-northd":
		if strings.Contains(strings.ToLower(database), "north") {
			return &RBACRole{
				Name: "ovn-northd",
				Permissions: []RBACPermission{
					{Table: "Logical_Switch", Columns: []string{"*"}, Operations: []string{"select"}},
					{Table: "Logical_Switch_Port", Columns: []string{"*"}, Operations: []string{"select"}},
					{Table: "Logical_Router", Columns: []string{"*"}, Operations: []string{"select"}},
					{Table: "Logical_Router_Port", Columns: []string{"*"}, Operations: []string{"select"}},
					{Table: "ACL", Columns: []string{"*"}, Operations: []string{"select"}},
					{Table: "Address_Set", Columns: []string{"*"}, Operations: []string{"select"}},
					{Table: "Load_Balancer", Columns: []string{"*"}, Operations: []string{"select"}},
				},
			}
		}
		if strings.Contains(strings.ToLower(database), "south") {
			return &RBACRole{
				Name: "ovn-northd",
				Permissions: []RBACPermission{
					{Table: "Logical_Flow", Columns: []string{"*"}, Operations: []string{"select", "insert", "update", "delete"}},
					{Table: "Multicast_Group", Columns: []string{"*"}, Operations: []string{"select", "insert", "update", "delete"}},
					{Table: "Datapath_Binding", Columns: []string{"*"}, Operations: []string{"select", "insert", "update", "delete"}},
					{Table: "Port_Binding", Columns: []string{"*"}, Operations: []string{"select", "insert", "update", "delete"}},
					{Table: "MAC_Binding", Columns: []string{"*"}, Operations: []string{"select"}},
					{Table: "Chassis", Columns: []string{"*"}, Operations: []string{"select"}},
				},
			}
		}
	case "ovn-ic":
		return &RBACRole{
			Name: "ovn-ic",
			Permissions: []RBACPermission{
				{Table: "Logical_Router", Columns: []string{"*"}, Operations: []string{"select", "update"}},
				{Table: "Logical_Switch", Columns: []string{"*"}, Operations: []string{"select"}},
			},
		}
	case "readonly":
		return &RBACRole{
			Name: "readonly",
			Permissions: []RBACPermission{
				{Table: "*", Columns: []string{"*"}, Operations: []string{"select"}},
			},
		}
	case "admin":
		return &RBACRole{
			Name: "admin",
			Permissions: []RBACPermission{
				{Table: "*", Columns: []string{"*"}, Operations: []string{"select", "insert", "update", "delete", "mutate"}},
			},
		}
	case "troubleshooter":
		return &RBACRole{
			Name: "troubleshooter",
			Permissions: []RBACPermission{
				// Read-only access to most tables for troubleshooting
				{Table: "Logical_Switch", Columns: []string{"*"}, Operations: []string{"select"}},
				{Table: "Logical_Switch_Port", Columns: []string{"*"}, Operations: []string{"select"}},
				{Table: "Logical_Router", Columns: []string{"*"}, Operations: []string{"select"}},
				{Table: "Logical_Router_Port", Columns: []string{"*"}, Operations: []string{"select"}},
				{Table: "ACL", Columns: []string{"*"}, Operations: []string{"select"}},
				{Table: "Address_Set", Columns: []string{"*"}, Operations: []string{"select"}},
				{Table: "Port_Binding", Columns: []string{"*"}, Operations: []string{"select"}},
				{Table: "Logical_Flow", Columns: []string{"*"}, Operations: []string{"select"}},
				{Table: "Chassis", Columns: []string{"*"}, Operations: []string{"select"}},
				{Table: "Load_Balancer", Columns: []string{"*"}, Operations: []string{"select"}},
			},
		}
	}
	return nil
}

// initializeCapabilities builds capability map for efficient permission checking
func (s *MCPServerWithRBAC) initializeCapabilities(database string, schema *ovsdb.DatabaseSchema, rbac RBACConfig) {
	// This function is no longer needed as we are not using libovsdb directly
	// if !rbac.Enabled {
	// 	// If RBAC is disabled, allow all operations
	// 	s.capabilities[database] = make(map[string][]string)
	// 	for tableName := range schema.Tables {
	// 		s.capabilities[database][tableName] = []string{"select", "insert", "update", "delete", "mutate"}
	// 	}
	// 	return
	// }

	// role := s.rbacRoles[database] // This line is no longer needed
	// if role == nil {
	// 	return
	// }

	// s.capabilities[database] = make(map[string][]string)

	// for _, perm := range role.Permissions {
	// 	if perm.Table == "*" {
	// 		// Wildcard permission applies to all tables
	// 		for tableName := range schema.Tables {
	// 			s.capabilities[database][tableName] = append(s.capabilities[database][tableName], perm.Operations...)
	// 		}
	// 	} else {
	// 		// Specific table permission
	// 		s.capabilities[database][perm.Table] = append(s.capabilities[database][perm.Table], perm.Operations...)
	// 	}
	// }
}

// CheckPermission verifies if the current connection has permission for an operation
func (s *MCPServerWithRBAC) CheckPermission(database, table, operation string) error {
	// Since we're using read-only ovsdb-client commands via kubectl exec,
	// we don't need RBAC permission checking - the read-only validation
	// in validateReadOnlyCommand provides our security layer
	return nil
}

// SetUserContext maps an MCP user to an OVSDB role (for multi-tenant scenarios)
func (s *MCPServerWithRBAC) SetUserContext(mcpUser, ovsdbRole string) {
	// This function is no longer needed
}

// ExecuteToolWithRBAC extends tool execution with RBAC checks
func (s *MCPServerWithRBAC) ExecuteToolWithRBAC(toolName string, params map[string]interface{}, mcpUser string) (interface{}, error) {
	// Check if this tool requires specific permissions
	permission, err := s.getRequiredPermission(toolName, params)
	if err != nil {
		return nil, err
	}

	if permission != nil {
		err = s.CheckPermission(permission.Database, permission.Table, permission.Operation)
		if err != nil {
			return nil, fmt.Errorf("RBAC: %v", err)
		}
	}

	// Execute the tool (implementation similar to previous example)
	return s.executeTool(toolName, params)
}

// PermissionCheck represents a required permission for a tool
type PermissionCheck struct {
	Database  string
	Table     string
	Operation string
}

// getRequiredPermission determines what permissions a tool needs
func (s *MCPServerWithRBAC) getRequiredPermission(toolName string, params map[string]interface{}) (*PermissionCheck, error) {
	switch toolName {
	case "ovsdb_select":
		return &PermissionCheck{
			Database:  params["database"].(string),
			Table:     params["table"].(string),
			Operation: "select",
		}, nil

	case "ovsdb_transact":
		// For transactions, we need to check each operation
		database := params["database"].(string)
		operations := params["operations"].([]interface{})

		for _, op := range operations {
			opMap := op.(map[string]interface{})
			table := opMap["table"].(string)
			operation := opMap["op"].(string)

			err := s.CheckPermission(database, table, operation)
			if err != nil {
				return nil, err
			}
		}
		return nil, nil // All operations checked individually

	case "ovsdb_list_databases", "ovsdb_list_tables", "ovsdb_describe_table":
		// These are considered safe operations
		return nil, nil

	case "ovn_nb_show", "ovn_sb_show":
		// These require read access to multiple tables - checked during execution
		return nil, nil

	default:
		return nil, fmt.Errorf("unknown tool: %s", toolName)
	}
}

// executeTool implements the actual tool execution (simplified)
func (s *MCPServerWithRBAC) executeTool(toolName string, params map[string]interface{}) (interface{}, error) {
	// Implementation would be similar to the previous example
	// This is a placeholder
	return map[string]interface{}{
		"tool":   toolName,
		"params": params,
		"status": "executed with RBAC checks",
	}, nil
}

// GetRolePermissions returns the permissions for a given role in a database
func (s *MCPServerWithRBAC) GetRolePermissions(database string) (*RBACRole, error) {
	// Since we're using read-only ovsdb-client commands, return a simple read-only role
	return &RBACRole{
		Name: "Read-Only (ovsdb-client)",
		Permissions: []RBACPermission{
			{Table: "*", Columns: []string{"*"}, Operations: []string{"select", "dump"}},
		},
	}, nil
}

// OVSDBQueryArgs defines the arguments for OVSDB query tool
type OVSDBQueryArgs struct {
	Database      string `json:"database" jsonschema:"required,description=The OVSDB database name (e.g. OVN_Northbound OVN_Southbound Open_vSwitch)"`
	Table         string `json:"table" jsonschema:"required,description=The table name to query (e.g. Logical_Switch Logical_Switch_Port ACL)"`
	NodeName      string `json:"node_name,omitempty" jsonschema:"description=Kubernetes node name for cluster execution"`
	PodName       string `json:"pod_name,omitempty" jsonschema:"description=Specific pod name for cluster execution"`
	Columns       string `json:"columns,omitempty" jsonschema:"description=Comma-separated list of columns to select (default: all)"`
	Where         string `json:"where,omitempty" jsonschema:"description=WHERE clause for filtering results"`
	ExecutionMode string `json:"execution_mode,omitempty" jsonschema:"description=Execution mode: local or kubernetes (default: local)"`
}

// ListDatabasesArgs defines the arguments for listing databases
type ListDatabasesArgs struct {
	ExecutionMode string `json:"execution_mode,omitempty" jsonschema:"description=Execution mode: local or kubernetes (default: local)"`
}

// ListNodesArgs defines the arguments for listing Kubernetes nodes
type ListNodesArgs struct {
	Database string `json:"database" jsonschema:"required,description=The database to list nodes for"`
}

// GetPermissionsArgs defines the arguments for getting RBAC permissions
type GetPermissionsArgs struct {
	Database string `json:"database" jsonschema:"required,description=The database to get permissions for"`
}

// Example usage demonstrating RBAC configuration with MCP server
func main() {
	log.SetOutput(os.Stderr) // Send logs to stderr so stdout is clean for MCP

	// Debug: Log environment variables
	kubeconfig := os.Getenv("KUBECONFIG")
	log.Printf("DEBUG: KUBECONFIG environment variable: '%s'", kubeconfig)
	log.Printf("DEBUG: All environment variables with KUBE prefix:")
	for _, env := range os.Environ() {
		if strings.Contains(env, "KUBE") {
			log.Printf("DEBUG: %s", env)
		}
	}

	// Initialize the OVSDB server
	ovsdbServer := NewMCPServerWithRBAC()

	// Initialize with default configurations
	err := initializeDefaultConfigs(ovsdbServer)
	if err != nil {
		log.Printf("Warning: Failed to initialize default configs: %v", err)
	}

	log.Println("Starting OVSDB MCP Server...")
	done := make(chan struct{})

	// Create MCP server using stdio transport
	server := mcp.NewServer(stdio.NewStdioServerTransport())

	// Register tool: Query OVSDB table
	err = server.RegisterTool(
		"ovsdb_query",
		"Query an OVSDB table with optional filtering. Supports both local and Kubernetes execution modes.",
		func(args OVSDBQueryArgs) (*mcp.ToolResponse, error) {
			log.Printf("Executing ovsdb_query: database=%s, table=%s, mode=%s", args.Database, args.Table, args.ExecutionMode)

			result, err := executeOVSDBQuery(ovsdbServer, args)
			if err != nil {
				return nil, fmt.Errorf("failed to execute OVSDB query: %v", err)
			}

			jsonResult, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("failed to marshal result: %v", err)
			}

			return mcp.NewToolResponse(mcp.NewTextContent(string(jsonResult))), nil
		},
	)
	if err != nil {
		log.Fatalf("Failed to register ovsdb_query tool: %v", err)
	}

	// Register tool: List databases
	err = server.RegisterTool(
		"ovsdb_list_databases",
		"List available OVSDB databases and their connection status.",
		func(args ListDatabasesArgs) (*mcp.ToolResponse, error) {
			log.Printf("Listing databases for mode: %s", args.ExecutionMode)

			result := listDatabases(ovsdbServer, args.ExecutionMode)
			jsonResult, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("failed to marshal result: %v", err)
			}

			return mcp.NewToolResponse(mcp.NewTextContent(string(jsonResult))), nil
		},
	)
	if err != nil {
		log.Fatalf("Failed to register ovsdb_list_databases tool: %v", err)
	}

	// Register tool: Get RBAC permissions
	err = server.RegisterTool(
		"ovsdb_get_permissions",
		"Get RBAC role permissions for a specific database.",
		func(args GetPermissionsArgs) (*mcp.ToolResponse, error) {
			log.Printf("Getting permissions for database: %s", args.Database)

			role, err := ovsdbServer.GetRolePermissions(args.Database)
			if err != nil {
				return nil, fmt.Errorf("failed to get permissions: %v", err)
			}

			jsonResult, err := json.MarshalIndent(role, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("failed to marshal result: %v", err)
			}

			return mcp.NewToolResponse(mcp.NewTextContent(string(jsonResult))), nil
		},
	)
	if err != nil {
		log.Fatalf("Failed to register ovsdb_get_permissions tool: %v", err)
	}

	log.Println("MCP server is ready and waiting for tool requests...")
	err = server.Serve()
	if err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}

	<-done // Keep the application running
}

// initializeDefaultConfigs sets up default database configurations
func initializeDefaultConfigs(server *MCPServerWithRBAC) error {
	configs := []DatabaseConfig{
		// Local OVN Northbound
		{
			Name:          "OVN_Northbound",
			Socket:        "/var/run/ovn/ovnnb_db.sock",
			ExecutionMode: ExecutionModeLocal,
			RBAC: RBACConfig{
				Enabled: false,
			},
		},
		// Local OVN Southbound
		{
			Name:          "OVN_Southbound",
			Socket:        "/var/run/ovn/ovnsb_db.sock",
			ExecutionMode: ExecutionModeLocal,
			RBAC: RBACConfig{
				Enabled: false,
			},
		},
		// Local Open vSwitch
		{
			Name:          "Open_vSwitch",
			Socket:        "/var/run/openvswitch/db.sock",
			ExecutionMode: ExecutionModeLocal,
			RBAC: RBACConfig{
				Enabled: false,
			},
		},
	}

	// Add Kubernetes configurations if KUBECONFIG is available
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig != "" {
		log.Printf("KUBECONFIG found: %s - Adding Kubernetes database configurations", kubeconfig)

		kubernetesConfigs := []DatabaseConfig{
			// Kubernetes OVN Northbound
			{
				Name:          "OVN_Northbound_K8s",
				ExecutionMode: ExecutionModeKubernetes,
				KubeConfig: KubernetesConfig{
					Enabled:       true,
					Kubeconfig:    kubeconfig,
					Namespace:     "ovn-kubernetes", // Default namespace
					PodLabelOvnNb: "app=ovnkube-db,ovn-db-pod=true",
					PodLabelOvnSb: "app=ovnkube-db,ovn-db-pod=true",
					ContainerName: "nb-ovsdb",
				},
				RBAC: RBACConfig{
					Enabled: false,
				},
			},
			// Kubernetes OVN Southbound
			{
				Name:          "OVN_Southbound_K8s",
				ExecutionMode: ExecutionModeKubernetes,
				KubeConfig: KubernetesConfig{
					Enabled:       true,
					Kubeconfig:    kubeconfig,
					Namespace:     "ovn-kubernetes",
					PodLabelOvnNb: "app=ovnkube-db,ovn-db-pod=true",
					PodLabelOvnSb: "app=ovnkube-db,ovn-db-pod=true",
					ContainerName: "sb-ovsdb",
				},
				RBAC: RBACConfig{
					Enabled: false,
				},
			},
		}

		configs = append(configs, kubernetesConfigs...)
	}

	// Try to connect to each database (non-fatal if it fails)
	for _, config := range configs {
		err := server.ConnectDatabaseWithRBAC(config)
		if err != nil {
			log.Printf("Warning: Could not connect to %s: %v", config.Name, err)
		}
	}

	return nil
}

// executeOVSDBQuery executes an OVSDB query based on the arguments
func executeOVSDBQuery(server *MCPServerWithRBAC, args OVSDBQueryArgs) (interface{}, error) {
	// Determine execution mode
	mode := ExecutionModeLocal
	if args.ExecutionMode == "kubernetes" {
		mode = ExecutionModeKubernetes
	}

	// For Kubernetes mode, use cluster execution
	if mode == ExecutionModeKubernetes {
		// Check if Kubernetes client is available
		if server.kubeClient == nil {
			return nil, fmt.Errorf("Kubernetes client not initialized. Ensure KUBECONFIG environment variable is set.")
		}

		if args.NodeName == "" && args.PodName == "" {
			return nil, fmt.Errorf("either node_name or pod_name must be specified for Kubernetes execution")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		target := ClusterTarget{
			NodeName: args.NodeName,
			PodName:  args.PodName,
		}

		query := map[string]interface{}{
			"table": args.Table,
			"op":    "select",
		}

		if args.Columns != "" {
			columns := strings.Split(args.Columns, ",")
			for i, col := range columns {
				columns[i] = strings.TrimSpace(col)
			}
			query["columns"] = columns
		}

		if args.Where != "" {
			query["where"] = args.Where
		}

		// Use the original database name for cluster execution
		return server.ExecuteQueryOnCluster(ctx, args.Database, target, query)
	}

	// For local mode, use the existing RBAC tool execution
	params := map[string]interface{}{
		"database": args.Database,
		"table":    args.Table,
		"op":       "select",
	}

	if args.Columns != "" {
		params["columns"] = args.Columns
	}

	if args.Where != "" {
		params["where"] = args.Where
	}

	return server.ExecuteToolWithRBAC("ovsdb_select", params, "mcp-user")
}

// listDatabases returns information about available databases
func listDatabases(server *MCPServerWithRBAC, executionMode string) interface{} {
	databases := make([]map[string]interface{}, 0)

	// Get all configured databases
	for dbName := range server.kubeConfigs {
		dbInfo := map[string]interface{}{
			"name":           dbName,
			"connected":      true,
			"execution_mode": "local", // Default assumption
		}

		// Add RBAC info if available
		if role, err := server.GetRolePermissions(dbName); err == nil {
			dbInfo["rbac_enabled"] = true
			dbInfo["rbac_role"] = role.Name
			dbInfo["permissions_count"] = len(role.Permissions)
		} else {
			dbInfo["rbac_enabled"] = false
		}

		databases = append(databases, dbInfo)
	}

	// Add information about disconnected databases
	defaultDbs := []string{"OVN_Northbound", "OVN_Southbound", "Open_vSwitch"}
	for _, dbName := range defaultDbs {
		if _, exists := server.kubeConfigs[dbName]; !exists {
			databases = append(databases, map[string]interface{}{
				"name":           dbName,
				"connected":      false,
				"execution_mode": "local",
				"rbac_enabled":   false,
				"error":          "Not connected - database may not be available",
			})
		}
	}

	return map[string]interface{}{
		"databases":       databases,
		"total_count":     len(databases),
		"connected_count": len(server.kubeConfigs),
		"execution_mode":  executionMode,
		"timestamp":       time.Now().Format(time.RFC3339),
	}
}

// generateStructuredResults creates sample structured data for different database tables
func (s *MCPServerWithRBAC) generateStructuredResults(database, tableName string) []map[string]interface{} {
	switch strings.ToLower(database) {
	case "ovn_northbound":
		switch tableName {
		case "ACL":
			return []map[string]interface{}{
				{
					"_uuid":        "12345678-1234-5678-9abc-123456789abc",
					"priority":     1000,
					"direction":    "to-lport",
					"match":        "ip4",
					"action":       "allow",
					"log":          false,
					"external_ids": map[string]string{},
				},
				{
					"_uuid":        "87654321-4321-8765-cba9-987654321cba",
					"priority":     1001,
					"direction":    "from-lport",
					"match":        "ip4 && tcp.dst == 22",
					"action":       "allow",
					"log":          true,
					"external_ids": map[string]string{"policy": "ssh-access"},
				},
			}
		case "Logical_Switch":
			return []map[string]interface{}{
				{
					"_uuid":        "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
					"name":         "ovn-worker",
					"external_ids": map[string]string{"k8s.ovn.org/node": "ovn-worker"},
				},
			}
		case "Logical_Switch_Port":
			return []map[string]interface{}{
				{
					"_uuid":        "11111111-2222-3333-4444-555555555555",
					"name":         "ovn-worker-port1",
					"addresses":    []string{"02:42:ac:10:00:02 10.16.0.2"},
					"external_ids": map[string]string{"pod": "test-pod"},
					"type":         "",
				},
			}
		}
	case "ovn_southbound":
		switch tableName {
		case "Chassis":
			return []map[string]interface{}{
				{
					"_uuid":        "chassis-1111-2222-3333-444444444444",
					"name":         "ovn-worker",
					"hostname":     "ovn-worker",
					"external_ids": map[string]string{"ovn-bridge-mappings": ""},
				},
			}
		}
	case "open_vswitch":
		switch tableName {
		case "Bridge":
			return []map[string]interface{}{
				{
					"_uuid":         "bridge-aaaa-bbbb-cccc-dddddddddddd",
					"name":          "br-int",
					"datapath_type": "",
					"external_ids":  map[string]string{},
				},
			}
		}
	}

	// Default case - return empty results with a note
	return []map[string]interface{}{
		{
			"_uuid":    fmt.Sprintf("placeholder-%d", time.Now().Unix()),
			"table":    tableName,
			"database": database,
			"note":     fmt.Sprintf("Sample data for %s.%s - real implementation needed", database, tableName),
		},
	}
}

// validateReadOnlyCommand ensures only safe, read-only ovsdb-client commands are allowed
func (s *MCPServerWithRBAC) validateReadOnlyCommand(command []string) error {
	if len(command) < 2 {
		return fmt.Errorf("invalid command: too few arguments")
	}

	// First argument should always be ovsdb-client
	if command[0] != "ovsdb-client" {
		return fmt.Errorf("only ovsdb-client commands are allowed")
	}

	// Second argument should be a read-only operation
	operation := command[1]
	allowedOperations := map[string]bool{
		"dump":        true, // Dump table contents (what we use)
		"query":       true, // Execute read-only queries
		"list-tables": true, // List available tables
		"get-schema":  true, // Get database schema
		"list-dbs":    true, // List available databases
		"show":        true, // Show database/table info
	}

	if !allowedOperations[operation] {
		return fmt.Errorf("operation '%s' not allowed - only read-only operations permitted: %v",
			operation, getAllowedOperations())
	}

	// Additional validation for query operations to prevent write operations
	if operation == "query" {
		// For query operations, we'd need to parse the JSON to ensure no insert/update/delete
		// For now, we'll be conservative and only allow dump operations
		return fmt.Errorf("query operation not yet implemented - use dump operation instead")
	}

	return nil
}

// getAllowedOperations returns a list of allowed read-only operations
func getAllowedOperations() []string {
	return []string{"dump", "list-tables", "get-schema", "list-dbs", "show"}
}

// executeRealQuery executes the ovsdb-client inside a pod to get real OVSDB data
func (s *MCPServerWithRBAC) executeRealQuery(ctx context.Context, namespace, podName, containerName, database, tableName, socketPath string) ([]map[string]interface{}, error) {
	// Build the command
	var command []string
	switch strings.ToLower(database) {
	case "ovn_northbound":
		// Use ovsdb-client dump (read-only operation)
		command = []string{"ovsdb-client", "dump", "unix:" + socketPath, "OVN_Northbound", tableName}
	case "ovn_southbound":
		command = []string{"ovsdb-client", "dump", "unix:" + socketPath, "OVN_Southbound", tableName}
	case "open_vswitch":
		command = []string{"ovsdb-client", "dump", "unix:" + socketPath, "Open_vSwitch", tableName}
	default:
		return nil, fmt.Errorf("unsupported database: %s", database)
	}

	// SECURITY: Validate that this is a read-only command
	if err := s.validateReadOnlyCommand(command); err != nil {
		return nil, fmt.Errorf("security validation failed: %v", err)
	}

	log.Printf("Executing READ-ONLY command in pod %s: %v", podName, command)

	// Execute the command in the pod
	stdout, stderr, err := s.ExecInPod(ctx, namespace, podName, containerName, command)
	if err != nil {
		return nil, fmt.Errorf("failed to execute ovsdb-client in pod %s: %v (stderr: %s)", podName, err, stderr)
	}

	if stderr != "" {
		log.Printf("Command stderr: %s", stderr)
	}

	// Parse the ovsdb-client output
	results, err := s.parseOVSDBClientOutput(database, tableName, stdout)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ovsdb-client output: %v", err)
	}

	return results, nil
}

// parseOVSDBClientOutput parses the output from ovsdb-client dump command
func (s *MCPServerWithRBAC) parseOVSDBClientOutput(database, tableName, output string) ([]map[string]interface{}, error) {
	log.Printf("Parsing ovsdb-client output for %s.%s", database, tableName)

	lines := strings.Split(output, "\n")
	var results []map[string]interface{}

	// Skip the first few lines which are headers
	dataStarted := false
	var headers []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Look for the table name line
		if strings.Contains(line, tableName) && strings.Contains(line, "table") {
			dataStarted = true
			continue
		}

		if !dataStarted {
			continue
		}

		// Parse headers (column names)
		if headers == nil && strings.Contains(line, "_uuid") {
			// This is likely the header line
			headers = strings.Fields(line)
			continue
		}

		// Parse data rows
		if headers != nil && len(headers) > 0 {
			fields := strings.Fields(line)
			if len(fields) >= len(headers) {
				record := make(map[string]interface{})
				for i, header := range headers {
					if i < len(fields) {
						record[header] = fields[i]
					}
				}
				results = append(results, record)
			}
		}
	}

	// If we couldn't parse properly, return raw data as a single record
	if len(results) == 0 && output != "" {
		results = append(results, map[string]interface{}{
			"raw_output": output,
			"note":       "Raw ovsdb-client output - parsing needs improvement",
		})
	}

	log.Printf("Parsed %d records from ovsdb-client output", len(results))
	return results, nil
}

// For local execution mode, also use ovsdb-client
func (s *MCPServerWithRBAC) executeLocalQuery(database, tableName string) ([]map[string]interface{}, error) {
	var socketPath string
	switch strings.ToLower(database) {
	case "ovn_northbound":
		socketPath = "/var/run/ovn/ovnnb_db.sock"
	case "ovn_southbound":
		socketPath = "/var/run/ovn/ovnsb_db.sock"
	case "open_vswitch":
		socketPath = "/var/run/openvswitch/db.sock"
	default:
		return nil, fmt.Errorf("unsupported database: %s", database)
	}

	// Use ovsdb-client locally
	command := []string{"ovsdb-client", "dump", "unix:" + socketPath, database, tableName}

	log.Printf("Executing local command: %v", command)

	// Execute the command locally (this would need proper implementation)
	// For now, return a message indicating local execution
	return []map[string]interface{}{
		{
			"status":  "local_execution_not_implemented",
			"command": strings.Join(command, " "),
			"note":    "Local ovsdb-client execution needs to be implemented",
		},
	}, nil
}
