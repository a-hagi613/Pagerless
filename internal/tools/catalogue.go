package tools

// JSON Schema keys shared by every catalogue entry.
const (
	fieldKind         = "kind"
	fieldAPIVersion   = "apiVersion"
	fieldName         = "name"
	fieldNamespace    = "namespace"
	fieldPrevious     = "previous"
	fieldSinceSeconds = "sinceSeconds"
	fieldLimit        = "limit"
	fieldConnector    = "connector"
	fieldRepository   = "repository"
	fieldPath         = "path"
	schemaType        = "type"
	schemaDescription = "description"
)

// The catalogue's stable tool names. They are also the only names the broker
// dispatches, so a request for anything else — a write verb, an exec, an
// invented tool — is refused before it can reach a backend.
const (
	// ToolObjectGet reads one Kubernetes object.
	ToolObjectGet = "object_get"
	// ToolObjectList lists Kubernetes objects of one kind.
	ToolObjectList = "object_list"
	// ToolPodsLogs reads pod logs, with the previous container first.
	ToolPodsLogs = "pods_logs"
	// ToolEventsList reads Events from both Event APIs.
	ToolEventsList = "events_list"
	// ToolOwnerChain walks an object's owner references up to its owner.
	ToolOwnerChain = "owner_chain"
	// ToolConnectorLogs reads logs from a configured log Connector.
	ToolConnectorLogs = "connector_logs"
	// ToolConnectorMetrics reads metrics from a configured metrics Connector.
	ToolConnectorMetrics = "connector_metrics"
	// ToolConnectorHealth reads a Connector's health and recent errors.
	ToolConnectorHealth = "connector_health"
	// ToolRepoTree lists a bounded part of the Target Repository tree.
	ToolRepoTree = "repo_tree"
	// ToolRepoFile reads one file from the Target Repository.
	ToolRepoFile = "repo_file"
	// ToolRepoCommits lists the Target Repository's recent commits.
	ToolRepoCommits = "repo_commits"
)

// catalogue is the fixed, ordered tool catalogue. The order is stable so that
// the manifest a server serves and the copy an agent checks in can be compared
// without normalising it.
func catalogue() []ToolSpec {
	return []ToolSpec{
		{
			Name: ToolObjectGet,
			Description: "Read one Kubernetes object in the Page's namespace. Returns the object's metadata, " +
				"status, and spec. Secrets and cluster-scoped kinds other than the investigated Node are refused.",
			InputSchema: objectSchema(map[string]any{
				fieldKind: stringProperty("The object's kind, such as Pod or Deployment."),
				fieldAPIVersion: stringProperty(
					"The object's API version, such as v1 or apps/v1; inferred from the kind when omitted."),
				fieldName:      stringProperty("The object's name."),
				fieldNamespace: stringProperty("Optional: the Page's namespace. Naming another namespace is refused."),
			}, fieldKind, fieldName),
		},
		{
			Name: ToolObjectList,
			Description: "List Kubernetes objects of one kind in the Page's namespace. Returns at most 50 objects. " +
				"Secrets and cluster-scoped kinds are refused.",
			InputSchema: objectSchema(map[string]any{
				fieldKind:       stringProperty("The objects' kind."),
				fieldAPIVersion: stringProperty("The objects' API version; inferred from the kind when omitted."),
				"labelSelector": stringProperty("An optional Kubernetes label selector."),
				fieldNamespace:  stringProperty("Optional: the Page's namespace. Naming another namespace is refused."),
			}, fieldKind),
		},
		{
			Name: ToolPodsLogs,
			Description: "Read the logs of the Target's pods, reading the previous container's log first. " +
				"A pod outside the Target is refused. Returns at most the requested tail lines of each container.",
			InputSchema: objectSchema(map[string]any{
				"pod":             stringProperty("Optional: one of the Target's pods."),
				"container":       stringProperty("Optional: one container of that pod."),
				fieldPrevious:     boolProperty("Read the previous container instance. Defaults to true."),
				"tailLines":       integerProperty("Lines to return per container, 1 to 1000. Defaults to 200."),
				fieldSinceSeconds: integerProperty("Look back this many seconds. Defaults to 3600."),
			}),
		},
		{
			Name: ToolEventsList,
			Description: "Read Events for the Target from both the core/v1 and events.k8s.io/v1 APIs, merged and " +
				"ordered oldest first. Events for another object are refused.",
			InputSchema: objectSchema(map[string]any{
				fieldSinceSeconds: integerProperty("Look back this many seconds. Defaults to 3600."),
				fieldLimit:        integerProperty("Maximum events to return, 1 to 200. Defaults to 100."),
			}),
		},
		{
			Name: ToolOwnerChain,
			Description: "Walk an object's owner references up to the object that owns it, such as " +
				"Pod to ReplicaSet to Deployment. Returns each owner's kind, name, namespace, and UID.",
			InputSchema: objectSchema(map[string]any{
				fieldKind:       stringProperty("The object's kind."),
				fieldAPIVersion: stringProperty("The object's API version; inferred from the kind when omitted."),
				fieldName:       stringProperty("The object's name."),
				fieldNamespace:  stringProperty("Optional: the Page's namespace. Naming another namespace is refused."),
			}, fieldKind, fieldName),
		},
		{
			Name: ToolConnectorLogs,
			Description: "Query one configured log Connector for the Target's logs. The server injects the Page's " +
				"namespace and the Target selector into every query, so the arguments cannot widen the scope.",
			InputSchema: objectSchema(map[string]any{
				fieldConnector:    stringProperty("The Connector's configured name."),
				"pod":             stringProperty("Optional: narrow to one of the Target's pods."),
				"container":       stringProperty("Optional: narrow to one container."),
				"filter":          stringProperty("Optional adapter-validated narrowing expression."),
				fieldSinceSeconds: integerProperty("Look back this many seconds. Defaults to 3600."),
				fieldLimit:        integerProperty("Maximum entries, 1 to 1000. Defaults to 200."),
			}, fieldConnector),
		},
		{
			Name: ToolConnectorMetrics,
			Description: "Query one configured metrics Connector for the Target's series. The server injects the " +
				"Page's namespace and the Target selector into every query.",
			InputSchema: objectSchema(map[string]any{
				fieldConnector:    stringProperty("The Connector's configured name."),
				"query":           stringProperty("A backend-native query, validated and bounded by the Connector."),
				fieldSinceSeconds: integerProperty("Look back this many seconds. Defaults to 3600."),
				"stepSeconds":     integerProperty("Resolution in seconds. Defaults to 60."),
				fieldLimit:        integerProperty("Maximum series, 1 to 50. Defaults to 20."),
			}, fieldConnector, "query"),
		},
		{
			Name: ToolConnectorHealth,
			Description: "Read a configured Connector's health: whether it is enabled, its recent failure count, " +
				"and the last error. Returns provider-shaped metadata only, never a credential.",
			InputSchema: objectSchema(map[string]any{
				fieldConnector: stringProperty("The Connector's configured name."),
			}, fieldConnector),
		},
		{
			Name: ToolRepoTree,
			Description: "List the Target Repository's tree under one path at the Page's ref. Returns paths and " +
				"blob sizes, at most 200 entries.",
			InputSchema: objectSchema(map[string]any{
				fieldRepository: stringProperty("Optional: the Page's Target Repository as owner/name. Naming another is refused."),
				fieldPath:       stringProperty("Optional path relative to the repository root. Empty lists the root."),
			}),
		},
		{
			Name: ToolRepoFile,
			Description: "Read one file from the Target Repository at the Page's ref. Returns the decoded text, " +
				"at most 64 KiB. Binary and oversized files are refused with a normal tool error.",
			InputSchema: objectSchema(map[string]any{
				fieldRepository: stringProperty("Optional: the Page's Target Repository as owner/name. Naming another is refused."),
				fieldPath:       stringProperty("The file's path relative to the repository root."),
			}, fieldPath),
		},
		{
			Name: ToolRepoCommits,
			Description: "List the Target Repository's recent commits at the Page's ref, optionally for one path. " +
				"Returns at most 20 commits, newest first.",
			InputSchema: objectSchema(map[string]any{
				fieldRepository: stringProperty("Optional: the Page's Target Repository as owner/name. Naming another is refused."),
				fieldPath:       stringProperty("Optional path the commits must touch."),
				fieldLimit:      integerProperty("Maximum commits, 1 to 20. Defaults to 10."),
			}),
		},
	}
}

// Names returns the catalogue's tool names in order.
func Names() []string {
	specs := catalogue()
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

// inCatalogue reports whether a name is a read tool this broker serves.
func inCatalogue(name string) bool {
	for _, spec := range catalogue() {
		if spec.Name == name {
			return true
		}
	}
	return false
}

// objectSchema builds an object schema with no extra properties.
func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		schemaType:             "object",
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProperty(description string) map[string]any {
	return map[string]any{schemaType: "string", schemaDescription: description}
}

func integerProperty(description string) map[string]any {
	return map[string]any{schemaType: "integer", schemaDescription: description}
}

func boolProperty(description string) map[string]any {
	return map[string]any{schemaType: "boolean", schemaDescription: description}
}
