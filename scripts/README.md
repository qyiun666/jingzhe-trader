# miniQMT Python Sidecar

为 Go 量化交易系统提供 miniQMT (迅投) 交易接口的 Python HTTP 桥接服务。

## 安装依赖

确保当前环境已安装 miniQMT 自带的 `xtquant` 库（通常位于 miniQMT 安装目录下），然后安装 Flask：

```bash
pip install -r requirements.txt
```

## 配置 miniQMT 路径

### 方式一：环境变量（推荐，支持启动时自动连接）

```bash
export QMT_PATH="/path/to/miniqmt/userdata_mini"
export QMT_ACCOUNT="1000000365"
export QMT_SESSION_ID="123456"   # 可选，默认 123456
export QMT_PORT="16888"          # 可选，默认 16888
```

### 方式二：运行时通过 HTTP 接口连接

启动后调用 `POST /connect` 传入连接信息（见下方 API 列表）。

## 启动 Sidecar

```bash
python qmt_sidecar.py
```

服务默认监听 `127.0.0.1:16888`，仅本机可访问。

## HTTP API 列表

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/health` | 健康检查，返回连接状态 |
| POST | `/connect` | 连接 miniQMT |
| POST | `/order` | 下单 |
| POST | `/cancel` | 撤单 |
| GET | `/positions` | 查询持仓 |
| GET | `/asset` | 查询资产 |
| GET | `/orders` | 查询当日委托 |
| GET | `/trades` | 查询当日成交 |

### 接口详情

#### POST /connect

请求体：

```json
{
  "path": "/path/to/userdata_mini",
  "session_id": 123456,
  "account_id": "1000000365"
}
```

响应：

```json
{"success": true, "message": "connected"}
```

#### POST /order

请求体：

```json
{
  "stock_code": "600000.SH",
  "order_type": "buy",
  "volume": 100,
  "price_type": "fix",
  "price": 10.5,
  "strategy_name": "",
  "remark": ""
}
```

- `order_type`: `buy` 或 `sell`
- `price_type`: `fix` (限价) 或 `latest` (最新价)

响应：

```json
{"success": true, "order_id": "12345"}
```

#### POST /cancel

请求体：

```json
{"order_id": "12345"}
```

响应：

```json
{"success": true}
```

#### GET /positions

响应：

```json
{
  "success": true,
  "positions": [
    {
      "stock_code": "600000.SH",
      "volume": 100,
      "avg_price": 10.5,
      "open_price": 10.5,
      "market_value": 1050.0
    }
  ]
}
```

#### GET /asset

响应：

```json
{
  "success": true,
  "cash": 100000.0,
  "total_asset": 200000.0,
  "market_value": 100000.0
}
```

#### GET /orders

响应：

```json
{
  "success": true,
  "orders": [
    {
      "order_id": "12345",
      "stock_code": "600000.SH",
      "order_type": "buy",
      "order_volume": 100,
      "traded_volume": 50,
      "price": 10.5,
      "order_status": "部成",
      "order_status_code": 55
    }
  ]
}
```

订单状态码映射：48-未报, 49-待报, 50-已报, 51-已报待撤, 52-部成待撤, 53-部撤, 54-已撤, 55-部成, 56-已成, 57-废单, 255-未知。

#### GET /trades

响应：

```json
{
  "success": true,
  "trades": [
    {
      "order_id": "12345",
      "stock_code": "600000.SH",
      "traded_volume": 50,
      "traded_price": 10.5,
      "trade_time": "2024-01-01 10:30:00"
    }
  ]
}
```

## 注意事项

- 本服务仅绑定 `127.0.0.1`，不可从外部网络直接访问。
- 所有接口均返回 JSON，异常时返回 `{"success": false, "error": "..."}`。
- 如果未连接 miniQMT，交易/查询接口会返回 503 状态码。
