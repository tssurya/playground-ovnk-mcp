# OVSDB MCP Server for OVN-Kubernetes

A Model Context Protocol (MCP) server that provides access to OVN (Open Virtual Network) databases running in Kubernetes clusters. Built specifically for use with AI assistants like Cursor to query and analyze OVN networking data.

## 🎯 What This Does

This MCP server allows you to query OVN databases directly from your Kubernetes cluster using natural language through Cursor. It executes `ovsdb-client` commands inside `ovnkube-node` pods to retrieve real, live data from your OVN deployment.

## ✨ Key Features

- **🔍 Real Data**: Queries actual OVN databases in your Kubernetes cluster (no mock data)
- **☸️ Kubernetes Native**: Uses `kubectl exec` to run queries inside OVN pods
- **🏠 Node Targeting**: Query databases on specific Kubernetes nodes
- **📊 Multiple Databases**: Access OVN Northbound, Southbound, and Open vSwitch databases
- **🤖 AI Ready**: Full MCP support for Cursor and other AI assistants
- **🛠️ Simple**: Uses standard `ovsdb-client` tools - no complex libovsdb setup

## 🏗️ How It Works

```
┌─────────────────┐    ┌──────────────────┐    ┌─────────────────────┐
│   Cursor IDE    │◄──►│  OVSDB MCP       │◄──►│  ovnkube-node pods  │
│   (AI Client)   │    │  Server          │    │  (Kubernetes)       │
└─────────────────┘    └──────────────────┘    └─────────────────────┘
                              │                           │
                              │                           ▼
                              │                  ┌─────────────────────┐
                              │                  │  ovsdb-client dump  │
                              │                  │  Unix sockets:      │
                              │                  │  - ovnnb_db.sock    │
                              │                  │  - ovnsb_db.sock    │
                              └─────────────────►│  - db.sock          │
                                kubectl exec     └─────────────────────┘
```

## 🚀 Quick Start

### Prerequisites

1. **OVN-Kubernetes cluster** with `ovnkube-node` pods running
2. **kubectl access** with permissions to:
   - List pods in `ovn-kubernetes` namespace
   - Execute commands in pods (`pods/exec`)
3. **Go 1.22+** for building
4. **Cursor IDE** or compatible MCP client

### Installation

1. **Clone and Build**
   ```bash
   git clone <your-repo>
   cd playground-ovnk-mcp
   go mod tidy
   go build -o ovsdb-mcp-server ovsdb-common-mcp-server.go
   ```

2. **Set Environment Variables**
   ```bash
   export KUBECONFIG=/path/to/your/kubeconfig
   ```

3. **Configure Cursor MCP**
   
   Create or update `~/.cursor/mcp.json` or project-local `mcp-config.json`:
   ```json
   {
     "mcpServers": {
       "ovsdb-rbac-server": {
         "command": "/absolute/path/to/playground-ovnk-mcp/ovsdb-mcp-server",
         "args": [],
         "cwd": "/absolute/path/to/playground-ovnk-mcp",
         "env": {
           "OVSDB_LOG_LEVEL": "info",
           "KUBECONFIG": "/path/to/your/kubeconfig"
         }
       }
     }
   }
   ```

4. **Restart Cursor** to load the new MCP server

## 🎮 Usage Examples

### List ACLs on a Specific Node

```
Use ovsdb_query with database="OVN_Northbound", table="ACL", execution_mode="kubernetes", and node_name="ovn-worker" to show all ACLs on the ovn-worker node
```

### Query Logical Switches

```
Use ovsdb_query with database="OVN_Northbound", table="Logical_Switch", execution_mode="kubernetes", and node_name="ovn-control-plane" to list all logical switches
```

### Check Chassis Information

```
Use ovsdb_query with database="OVN_Southbound", table="Chassis", execution_mode="kubernetes", and node_name="ovn-worker2" to show chassis details
```

### List Available Databases

```
Use ovsdb_list_databases with execution_mode="kubernetes" to see what databases are available
```

## 📊 Supported Databases & Tables

The server can query **any table** in these databases using `ovsdb-client dump`:

### OVN Northbound (`OVN_Northbound`)
- **Container**: `nb-ovsdb` 
- **Socket**: `/var/run/ovn/ovnnb_db.sock`
- **Common Tables**: `ACL`, `Logical_Switch`, `Logical_Switch_Port`, `Logical_Router`, `Logical_Router_Port`, `Load_Balancer`, `Address_Set`

### OVN Southbound (`OVN_Southbound`) 
- **Container**: `sb-ovsdb`
- **Socket**: `/var/run/ovn/ovnsb_db.sock` 
- **Common Tables**: `Chassis`, `Port_Binding`, `Logical_Flow`, `Multicast_Group`, `Datapath_Binding`, `MAC_Binding`

### Open vSwitch (`Open_vSwitch`)
- **Container**: `ovnkube-controller`
- **Socket**: `/var/run/openvswitch/db.sock`
- **Common Tables**: `Bridge`, `Interface`, `Port`, `Controller`, `Manager`

## 🛠️ MCP Tools

### `ovsdb_query`
Query any OVSDB table in your Kubernetes cluster.

**Required Parameters:**
- `database`: Database name (`OVN_Northbound`, `OVN_Southbound`, `Open_vSwitch`)
- `table`: Table name (any valid OVSDB table)
- `execution_mode`: Set to `"kubernetes"` for cluster queries
- `node_name`: Kubernetes node name to target

**Optional Parameters:**
- `pod_name`: Specific pod name (instead of node_name)
- `columns`: Comma-separated column list (not implemented yet)
- `where`: WHERE clause filtering (not implemented yet)

### `ovsdb_list_databases`
List available databases and connection status.

**Parameters:**
- `execution_mode`: `"kubernetes"` or `"local"`

## 🔧 How Kubernetes Execution Works

When you use `execution_mode: "kubernetes"`, the server:

1. **🔍 Discovers Pods**: Finds `ovnkube-node` pods using label selector:
   ```
   app=ovnkube-node,ovn-db-pod=true
   ```

2. **🎯 Targets Node**: Locates the pod running on your specified `node_name`

3. **📦 Selects Container**: Automatically chooses the right container:
   - `OVN_Northbound` → `nb-ovsdb` container
   - `OVN_Southbound` → `sb-ovsdb` container  
   - `Open_vSwitch` → `ovnkube-controller` container

4. **⚡ Executes Query**: Runs this command inside the pod:
   ```bash
   kubectl exec -n ovn-kubernetes <pod-name> -c <container> -- \
     ovsdb-client dump unix:/var/run/ovn/ovnnb_db.sock OVN_Northbound <table>
   ```

5. **📋 Parses Results**: Converts raw `ovsdb-client` output to structured JSON

## 🐛 Troubleshooting

### "Kubernetes client not initialized"
**Problem**: KUBECONFIG not detected
**Solution**:
```bash
# Verify KUBECONFIG is set
echo $KUBECONFIG

# Check your kubeconfig works
kubectl get nodes

# Restart Cursor after updating MCP config
```

### "No ovnkube-node pods found on node X"
**Problem**: Pod discovery failing
**Solution**:
```bash
# Check if pods exist
kubectl get pods -n ovn-kubernetes -l app=ovnkube-node,ovn-db-pod=true

# Verify node names
kubectl get nodes

# Check if pods are running on the target node
kubectl get pods -n ovn-kubernetes -l app=ovnkube-node,ovn-db-pod=true -o wide
```

### "Failed to execute ovsdb-client in pod"
**Problem**: Container or socket path issues
**Solution**:
```bash
# Check pod containers
kubectl describe pod -n ovn-kubernetes <ovnkube-node-pod-name>

# Verify socket exists
kubectl exec -n ovn-kubernetes <pod-name> -c nb-ovsdb -- ls -la /var/run/ovn/

# Test ovsdb-client manually
kubectl exec -n ovn-kubernetes <pod-name> -c nb-ovsdb -- ovsdb-client dump unix:/var/run/ovn/ovnnb_db.sock OVN_Northbound ACL
```

### "Permission denied" or "Connection refused"
**Problem**: Kubernetes RBAC permissions
**Solution**:
```bash
# Check your permissions
kubectl auth can-i list pods -n ovn-kubernetes
kubectl auth can-i create pods/exec -n ovn-kubernetes

# Verify pod is healthy
kubectl get pod -n ovn-kubernetes <pod-name> -o wide
```

### Data Parsing Issues
**Current Status**: The server retrieves real data but column parsing needs improvement.

**What works**: 
- ✅ Real data from your cluster
- ✅ Correct pod/container targeting
- ✅ Successful `ovsdb-client` execution

**What needs work**:
- 🔧 Better parsing of `ovsdb-client dump` output format
- 🔧 Proper column alignment and data structure

## 🧪 Manual Testing

Test the server directly:
```bash
# Test database listing
echo '{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "ovsdb_list_databases", "arguments": {"execution_mode": "kubernetes"}}}' | ./ovsdb-mcp-server

# Test ACL query
echo '{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "ovsdb_query", "arguments": {"database": "OVN_Northbound", "table": "ACL", "execution_mode": "kubernetes", "node_name": "ovn-worker"}}}' | ./ovsdb-mcp-server
```

## 📁 Project Structure

```
├── ovsdb-common-mcp-server.go  # Main server implementation
├── mcp-config.json            # Example MCP configuration  
├── README.md                  # This file
└── go.mod                     # Go dependencies
```

## 🔄 Current Implementation Details

### What We Use
- **Standard Tools**: `ovsdb-client dump` (pre-installed in OVN pods)
- **Simple Approach**: Direct `kubectl exec` execution
- **Real Data**: Live queries against actual OVSDB instances
- **Kubernetes Native**: Leverages existing OVN-Kubernetes deployment

### What We Don't Use
- ❌ Complex `libovsdb` Go client setup
- ❌ Port-forwarding or TCP connections  
- ❌ Generated model structs
- ❌ Mock or sample data
- ❌ Complex RBAC implementation (simplified for now)

## 🚀 Future Improvements

1. **Better Parsing**: Improve `ovsdb-client dump` output parsing for cleaner JSON
2. **More Tools**: Add tools for specific OVN operations (flows, routes, etc.)
3. **Local Mode**: Support for local Unix socket connections
4. **Filtering**: Implement `WHERE` clause and column selection
5. **RBAC**: Add back role-based access control if needed

## 🤝 Contributing

1. Fork the repository
2. Create a feature branch
3. Test with a real OVN-Kubernetes cluster
4. Submit a pull request

## 📚 Related Projects

- [OVN-Kubernetes](https://github.com/ovn-org/ovn-kubernetes) - Kubernetes CNI using OVN
- [OVN](https://www.ovn.org/) - Open Virtual Network
- [MCP Protocol](https://modelcontextprotocol.io/) - Model Context Protocol specification
- [Cursor](https://cursor.sh/) - AI-powered code editor

---

**Note**: This server is designed specifically for querying OVN databases in Kubernetes environments. It provides real, live data from your cluster's networking layer, making it perfect for troubleshooting, analysis, and understanding your OVN-Kubernetes deployment. 