package pveapi

type VersionInfo struct {
	Version string `json:"version"`
	Release string `json:"release"`
	RepoID  string `json:"repoid"`
}

type Resource struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	Name     string `json:"name"`
	VMID     int    `json:"vmid"`
	Node     string `json:"node"`
	Template int    `json:"template"`
}

type VMInfo struct {
	VMID int
	Node string
	Name string
	UUID string
}

type VMQuery struct {
	VMID         int
	ExpectedUUID string
	ExpectedName string
}
