# PVE 權限與新增 Kubernetes 節點

CSI 需要儲存池與 VM 的操作權限，也需要看完整的背景工作，才能確認上次的建刪／接拔操作是否仍在執行。

## 一次設定：把 Kubernetes VM 分組

1. PVE 網頁 → **Datacenter → Permissions → Pools → Create**，名稱填 `k8s`。
2. 左上角切換 **Pool View** → 選 `k8s` → **Members → Add → Virtual Machine**。
3. 加入 VM100（`k3s-master-01`）與 VM101（`k3s-worker-01`）。

Resource Pool 是 VM 分組。`data-ssd` 是儲存磁碟的地方，兩者不同；這個分組不需要加入 `data-ssd`。

## 一次設定：允許 CSI 接碟／拔碟

**Datacenter → Permissions → Roles → Create**，角色名稱填 `CSIK8sNode`，選取：

| 權限 | 用途 |
| --- | --- |
| `VM.Audit` | 查看 VM 設定與已接上的磁碟 |
| `VM.Config.Disk` | 修改 VM 的磁碟設定，包括接碟／拔碟 |

在 **Datacenter → Permissions → Add** 新增以下兩筆，兩筆的 **Propagate（傳播）都勾選**：

| 類型 | Path | 對象 | Role |
| --- | --- | --- | --- |
| User Permission | `/pool/k8s` | `pve-csi@pve` | `CSIK8sNode` |
| API Token Permission | `/pool/k8s` | `pve-csi@pve!csi-dev` | `CSIK8sNode` |

Token 開啟 Privilege Separation 時，實際權限是帳號與 Token 都有的部分，所以兩邊都要設定。
Pool 成員會繼承權限；不用再為每個 VMID 各加兩筆。
`VM.Config.Disk` 能修改該 VM 的所有磁碟設定；程式仍必須檢查磁碟身分，不能把 PVE 權限視為只限 CSI 磁碟。

## 原有權限保留

| 路徑／權限 | 用途 |
| --- | --- |
| `/storage/data-ssd`：`Datastore.Audit` | 查看儲存池狀態與容量 |
| `/storage/data-ssd`：`Datastore.AllocateSpace` | 配置磁碟空間 |
| `/storage/data-ssd`：`Datastore.Allocate` | 刪除磁碟所需；此權限也能管理儲存設定 |
| `/vms/7777`：`VM.Config.Disk` | 查閱歸屬 7777 的磁碟內容所需 |
| `/vms`：`VM.Audit`，傳播開啟 | 完整查看 VM 清單／設定，辨識節點及檢查磁碟引用 |
| `/nodes/pve`：`Sys.Audit` | 查看該 PVE 主機上所有帳號／Token 的背景工作，避免把看不到誤當成沒有 |

上述權限同樣需要帳號與 Token 都具備。既有角色若已包含就不必重加。
7777 是預留的磁碟歸屬編號，不能建立成真實 VM 或容器；它不是 Kubernetes node 的 VMID。
全域 VM 查看權限不能被更細的 `NoAccess` 隱藏，否則程式無法確認完整清單。
新加的 Pool 權限負責限制「哪些 VM 可以修改磁碟」，並不取代全域唯讀檢查。

若缺少工作查看權限：在 **Datacenter → Permissions → Roles** 建立 `CSITaskAudit`，只選 `Sys.Audit`。再於 **Permissions → Add**，對 `/nodes/pve` 分別加入帳號與 API Token 的 `CSITaskAudit` 權限。這裡的 `pve` 換成你的 `PVE_NODE`；只需該路徑，不必開傳播。這個權限用來查看，不允許停止工作或修改主機。

Controller 啟動時就會檢查這個權限與 `/vms` 的 `VM.Audit` 傳播權限；不足即報錯退出，不會先開放 CSI。之後查詢工作清單前後仍會再次確認；不足時回 `PermissionDenied` 並指出 `Sys.Audit on /nodes/<主機>`。即使同一個帳號換 Token，也不能依靠只看自己工作的預設行為。

## 以後新增 node

1. 建立 PVE VM 時，**Resource Pool 選 `k8s`**；既有 VM 也可從 Pool 的 Members 加入。
2. 讓 VM 加入 Kubernetes。每台 VM 必須有自己的 SMBIOS UUID，複製 VM 時不要沿用舊 UUID。
3. CSI DaemonSet 在符合部署條件的 Linux node 啟動後，使用 VM UUID 自動辨識對應 VMID，不必改程式或重新 build。

無關 VM 沒有 UUID 不影響比對；Kubernetes VM 自己必須有 UUID。讀取失敗、沒有權限、UUID 格式錯誤或兩台使用相同 UUID，仍會拒絕接碟，避免選錯機器。刪碟前的引用檢查仍包含所有 VM、模板與容器。

目前儲存限制為設定的單一 `PVE_NODE`，新 VM 必須能存取 StorageClass 指定的儲存池；加入 Pool 不會讓本機 LVM-thin 變成跨 PVE 主機共享儲存。

## 設定完成後確認

用 CSI 的 Token 查 `/access/permissions?path=/vms/100` 與 `/vms/101`，兩者都應包含 `VM.Audit`、`VM.Config.Disk`。
另外查 `/access/permissions?path=/nodes/pve`，應包含 `Sys.Audit`。
權限查詢中值 `0`／`1` 表示是否向下傳播，不是「拒絕／允許」；權限名稱有出現才代表具備該權限。
只確認權限不會接碟、拔碟或修改 VM。

依據：[PVE 官方使用者、Token 與 Pool 權限文件](https://github.com/proxmox/pve-docs/blob/master/pveum.adoc)、[PVE 工作清單的權限過濾](https://github.com/proxmox/pve-manager/blob/master/PVE/API2/Tasks.pm)。
