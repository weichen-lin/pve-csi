# 檢查 PVE Token 與部署條件

`pve-csi check` 使用現有 CSI Token，從 macOS 或 Linux 連到 PVE API，檢查權限、儲存池與指定 VM 的準備狀態。只送出讀取請求，不啟動 CSI、不建立資源，也不需要 PVE root 帳號或在 PVE 主機上執行。

請使用既有帳號與 API Token，權限請依 [PVE 權限設定](PVE-SETUP.md) 準備。這個指令負責驗證既有設定，不會建立或修正它們。

## 使用 .env 執行

在本專案根目錄執行，並安裝符合 `go.mod` 的 Go 版本（目前為 Go 1.27.2）。本機必須能連到 PVE 的 HTTPS API。

先複製範例設定：

```sh
cp .env.example .env
```

編輯 `.env`，把所有範例值換成自己的設定，包括 Token secret。`.env` 已被 Git 忽略，只有不含真實憑證的 [`.env.example`](.env.example) 會納入版本控制。

接著編譯並執行：

```sh
go build -o bin/pve-csi ./cmd/pve-csi
./bin/pve-csi check
```

`check` 會自動讀取**目前工作目錄**的 `.env`，適用 fish、zsh 與 Bash，不需要 shell 載入指令。若已匯出同名且非空的環境變數，會優先使用環境變數；空值則使用檔案中的值。`.env` 不存在時，也能完全使用已匯出的環境變數執行。

所有設定都放在 `.env` 或環境變數中。若要直接執行原始碼，使用 `go run ./cmd/pve-csi check`。編譯好的執行檔可在對應作業系統與架構上執行，不需要 Go。查詢說明使用 `./bin/pve-csi check --help` 或 `-h`。

設定檔每行一個 `KEY=value`，支援引號、`#` 註解與選填的 `export` 前綴。值只能佔一行；程式不展開變數，也不執行 shell 指令，路徑請直接填入實際值。

## 設定欄位

| 輸入 | 意義與範例 |
| --- | --- |
| `PVE_ENDPOINT` | PVE HTTPS API 位址；`pve.example.com` 是範例網域。預設會驗證主機名稱或 IP 是否符合憑證。 |
| `PVE_TOKEN_ID` | 完整 Token ID，格式為 `帳號@realm!Token名稱`。範例的帳號是 `pve-csi@pve`，Token 名稱是 `csi-dev`。 |
| `PVE_TOKEN_SECRET` | 建立 API Token 時取得的 secret；請替換範例的 `replace-with-token-secret`。 |
| `PVE_NODE` | PVE UI 顯示的主機名稱；範例 `pve` 不是 Kubernetes node 名稱。 |
| `PVE_OWNER_VMID` | CSI 預留的磁碟歸屬編號；範例 `7777` 必須尚未被真實 VM 或容器使用。 |
| `PVE_CA_FILE` | 選填，本機 PEM CA 憑證路徑。留空或未設定時使用系統信任的 CA。 |
| `PVE_STORAGE` | 要供 CSI 使用的既有 PVE storage ID；`data-ssd` 是範例。 |
| `PVE_VMIDS` | 預期作為 Kubernetes 節點的 PVE VMID，用逗號分隔，例如 `100,101`。至少提供一個 VMID，不可重複 ID 或使用磁碟歸屬編號。 |
| `PVE_CHECK_TIMEOUT` | 整次檢查的時間上限，預設 `60s`，例如 `90s`。 |
| `PVE_CHECK_INSECURE` | 僅供 `check` 暫時略過伺服器憑證驗證，預設 `false`。使用 `true`／`false`。 |

這些值應與部署設定一致：Helm 的 `pve.endpoint`、`pve.node`、`pve.ownerVMID`、`storageClass.storage`，以及 `pve.existingSecret` 內的兩個 Token 欄位。檢查不需要設定 `CSI_DRIVER_NAME` 或注入 build version。

已有 CSI PV 時，`PVE_OWNER_VMID` 必須沿用原本的磁碟歸屬編號。若檢查指出它被真實 VM／容器占用，先釐清衝突，不要為了讓檢查通過而直接更換部署的 owner VMID。

私有 CA 憑證必須來自可信任的 PVE 安裝；只需公開的 CA 憑證，不需要私鑰。預設會驗證憑證鏈、有效期，以及網址的主機名稱／IP 是否符合憑證。

暫時略過這些驗證時，在 `.env` 設定 `PVE_CHECK_INSECURE='true'`。此模式仍使用 HTTPS，但無法驗證伺服器身分，每次都會顯示 `WARN`；改回 `false` 即恢復驗證。此設定只影響 `check`，Controller 的 TLS 驗證不變。

## 如何讀取結果

開始檢查時，報告開頭會列出實際使用的 API 位址、Token ID、PVE 主機、儲存池、owner VMID 與目標 VMID，不會顯示 Token secret。若這些值與 `.env` 不同，先確認終端機是否已匯出同名環境變數。

接著每個檢查項目會顯示狀態、對象與說明，最後列出統計：

| 狀態 | 意義 |
| --- | --- |
| `PASS` | 該項讀取或條件檢查通過。 |
| `FAIL` | 必要條件不符，例如權限不足、儲存池不可用或 VM 設定不符；依說明修正後重跑。 |
| `WARN` | 檢查範圍的限制或需要留意的事項，不會單獨造成失敗。 |
| `SKIP` | 因前置條件失敗等原因，該項未完成；不能當成通過。 |

有必要檢查失敗、未完成或指令錯誤時，程序以非零狀態結束；只有警告時仍可成功。若驗證失敗，先核對 Token ID／secret；若權限不足，依 [PVE 權限設定](PVE-SETUP.md) 同時檢查帳號與 Token 的權限。

`storage_read_permissions` 通過表示有效權限符合讀取所需條件；實際儲存池狀態與內容能否讀取，分別由 `storage_status`、`storage_content` 驗證。權限檢查通過仍可能遇到後續 API 讀取失敗。

指定的 PVE 主機或 VM 不在目前可見清單中時，會指出找不到或無法看見該資源。請核對 API 位址、`PVE_NODE`、`PVE_VMIDS` 與 Token 權限；清單可能被權限過濾，不能只憑這個結果斷定資源不存在。

檢查涵蓋 API 連線與驗證、工作清單權限、VM 查看權限、PVE 主機、磁碟歸屬編號是否可用、儲存池狀態與權限，以及指定 VM 的 UUID、磁碟操作權限與接碟準備條件。

儲存容量至少須容納最小的 1 GiB CSI 磁碟；通過不代表足以配置更大的 PVC。主機、儲存名稱與 owner VMID 的組合也必須符合 CSI 磁碟識別碼的 128-byte 上限。

Token 能看到的清單可能受權限過濾，較細的 `NoAccess` 仍可能隱藏其他 VM 或容器。因此必須在 `PVE_VMIDS` 明確列出預期使用的每台 VM；全部通過也不能證明全叢集沒有隱藏資源或 UUID 衝突。

這是 PVE 端的唯讀檢查，不會實際建刪磁碟、接拔磁碟或測試 I/O，也不會連到 Kubernetes 比對節點 UUID、檢查 guest 核心或驗證掛載。部署後仍需用實際 PVC 與工作負載驗證完整流程。

## 程式分層與啟動模式

同一個 `pve-csi` 執行檔提供四個子命令：`controller`、`node` 是持續執行的 CSI 服務；`probe`、`check` 執行一次後結束。容器可透過 `args: [check]` 選擇檢查模式，設定仍從環境變數讀取。Kubernetes Job 是執行這個命令的外部資源，不是程式內另一個啟動模式。

## 在叢集內執行

Helm values 設定 `check.enabled: true` 與 `check.vmids` 後，chart 會建立暫停排程的 CronJob，它只當作範本，不會自動執行。release 名稱不超過 46 字元時，CronJob 名稱為 `<release>-check`；更長的名稱會縮短並加上雜湊，避免超過 52 字元限制。設定直接沿用 chart 的 PVE、Secret、CA 與 `storageClass.storage`，不需要 `.env`。

例如 release 與 namespace 都是 `pve-csi` 時，建立一次性 Job 並查看結果：

```sh
kubectl -n pve-csi create job pve-csi-check-001 --from=cronjob/pve-csi-check
kubectl -n pve-csi logs -f job/pve-csi-check-001
```

每次使用新的 Job 名稱。其他 release 可用下列 label 查出實際 CronJob 名稱；請把 namespace 與 instance label 換成自己的設定，再將查到的名稱填入 `--from=cronjob/` 後方：

```sh
kubectl -n pve-csi get cronjob -l 'app.kubernetes.io/instance=pve-csi,app.kubernetes.io/component=check' -o jsonpath='{.items[0].metadata.name}{"\n"}'
```

CronJob 最多保留一個成功與一個失敗的 Job；同類結果再次完成時，較早的 Job 與 Pod logs 可能在 TTL 到期前就被刪除。`check.ttlSecondsAfterFinished` 預設為 `86400`，表示完成一天後可由 TTL 機制清理，不保證至少保留一天，也不保證準時刪除。詳見 [chart 說明](charts/pve-csi/README.md#on-demand-pve-check)。

檢查流程依現有 DDD 分層組裝：

```text
cmd/pve-csi/check.go：讀設定、組裝依賴、顯示報告與退出狀態
  → volume.CheckService：檢查順序、前置條件、領域規則與結果彙整
    → volume.CheckGateway：唯讀查詢介面
      → pve.CheckGateway：PVE API、ACL、TLS 與 VM 設定解析
```

`CheckService` 與建刪磁碟的 `volume.Service` 分開，只依賴唯讀介面。owner、容量與 UUID 的判斷沿用 `domain` 規則；PVE adapter 回傳觀察結果及安全的錯誤分類，不決定整份報告的執行順序。介面、選項與報告型別由 `volume` 定義，`pve` 實作介面，`volume` 不依賴 `pve`。
