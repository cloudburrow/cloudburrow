package logagent

import (
	"fmt"
	"strconv"
)

// ConfigManifest is the fluentbit-gke-config ConfigMap holding cfg.
func ConfigManifest(cfg Config) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
  labels:
    k8s-app: %s
    addonmanager.kubernetes.io/mode: Reconcile
data:
  %s: %s
`, ConfigMap, Namespace, Name, ConfigKey, strconv.Quote(cfg.Marshal()))
}

// Manifest is the agent's objects: its ServiceAccount, the RBAC to read the
// pods and its ConfigMap, and the DaemonSet of image (pulled never: it is
// loaded into each node) writing to endpoint. Every node runs one, the
// control plane's too, whose components' logs are shipped from it.
func Manifest(image, endpoint string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    k8s-app: %[1]s
    addonmanager.kubernetes.io/mode: Reconcile
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: %[1]s
  labels:
    k8s-app: %[1]s
    addonmanager.kubernetes.io/mode: Reconcile
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: %[1]s
  labels:
    k8s-app: %[1]s
    addonmanager.kubernetes.io/mode: Reconcile
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: %[1]s
subjects:
  - kind: ServiceAccount
    name: %[1]s
    namespace: %[2]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    k8s-app: %[1]s
    addonmanager.kubernetes.io/mode: Reconcile
rules:
  - apiGroups: [""]
    resources: [configmaps]
    resourceNames: [%[3]s]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    k8s-app: %[1]s
    addonmanager.kubernetes.io/mode: Reconcile
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: %[1]s
subjects:
  - kind: ServiceAccount
    name: %[1]s
    namespace: %[2]s
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    k8s-app: %[1]s
    addonmanager.kubernetes.io/mode: Reconcile
spec:
  selector:
    matchLabels:
      k8s-app: %[1]s
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 100%%
  template:
    metadata:
      labels:
        k8s-app: %[1]s
        component: fluentbit-gke
    spec:
      serviceAccountName: %[1]s
      priorityClassName: system-node-critical
      terminationGracePeriodSeconds: 10
      tolerations:
        - operator: Exists
      containers:
        - name: %[1]s
          image: %[4]s
          imagePullPolicy: Never
          env:
            - name: NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: LOGGING_ENDPOINT
              value: %[5]q
          resources:
            requests:
              cpu: 5m
              memory: 20Mi
            limits:
              memory: 200Mi
          securityContext:
            runAsUser: 0
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
              add: [DAC_READ_SEARCH]
          volumeMounts:
            - name: varlog
              mountPath: /var/log
              readOnly: true
            - name: positions
              mountPath: %[6]s
      volumes:
        - name: varlog
          hostPath:
            path: /var/log
        - name: positions
          hostPath:
            path: %[6]s
            type: DirectoryOrCreate
`, Name, Namespace, ConfigMap, image, endpoint, PositionDir)
}
