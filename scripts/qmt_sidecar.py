#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
miniQMT Python Sidecar
======================
通过 Flask HTTP 服务为 Go 程序提供 miniQMT (xtquant) 交易接口桥接。
默认监听 127.0.0.1:16888，仅本机可访问。
"""

import os
import sys
import logging
from datetime import datetime
from typing import Any, Dict, List, Optional

from flask import Flask, jsonify, request

# ---------------------------------------------------------------------
# xtquant 导入（在 miniQMT 环境中可用）
# ---------------------------------------------------------------------
try:
    from xtquant.xttrader import XtQuantTrader, XtQuantTraderCallback
    from xtquant.xttype import StockAccount
    from xtquant import xtconstant
    XTQUANT_AVAILABLE = True
except ImportError:
    XTQUANT_AVAILABLE = False
    # 占位，用于类型提示不报错
    XtQuantTrader = Any
    XtQuantTraderCallback = object
    StockAccount = Any
    xtconstant = Any

# ---------------------------------------------------------------------
# 常量
# ---------------------------------------------------------------------
DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 16888

# 订单状态码 -> 中文描述
ORDER_STATUS_MAP = {
    48: "未报",
    49: "待报",
    50: "已报",
    51: "已报待撤",
    52: "部成待撤",
    53: "部撤",
    54: "已撤",
    55: "部成",
    56: "已成",
    57: "废单",
    255: "未知",
}

# 买卖方向映射
ORDER_TYPE_MAP = {
    "buy": 23,   # xtconstant.STOCK_BUY
    "sell": 24,  # xtconstant.STOCK_SELL
}

# 价格类型映射
PRICE_TYPE_MAP = {
    "fix": 11,    # xtconstant.FIX_PRICE
    "latest": 5,  # xtconstant.LATEST_PRICE
}

# ---------------------------------------------------------------------
# 日志配置
# ---------------------------------------------------------------------
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
logger = logging.getLogger("qmt_sidecar")

# ---------------------------------------------------------------------
# Flask 应用
# ---------------------------------------------------------------------
app = Flask(__name__)

# ---------------------------------------------------------------------
# 全局状态
# ---------------------------------------------------------------------
_g_xt_trader: Optional[XtQuantTrader] = None
_g_account: Optional[StockAccount] = None
_g_callback: Optional[XtQuantTraderCallback] = None
_g_connected: bool = False
_g_account_id: Optional[str] = None


# ---------------------------------------------------------------------
# 回调实现
# ---------------------------------------------------------------------
class QmtCallback(XtQuantTraderCallback):
    """miniQMT 推送回调，仅做日志记录，不做业务处理。"""

    def on_stock_order(self, order):
        logger.info(
            "[推送] 委托回报 %s | 状态=%s(%s) | 委托量=%s | 已成=%s | 价格=%s",
            getattr(order, "stock_code", "?"),
            getattr(order, "order_status", "?"),
            ORDER_STATUS_MAP.get(getattr(order, "order_status", 255), "?"),
            getattr(order, "order_volume", 0),
            getattr(order, "traded_volume", 0),
            getattr(order, "price", 0.0),
        )

    def on_stock_trade(self, trade):
        logger.info(
            "[推送] 成交回报 %s | 成交量=%s | 成交价=%s | 时间=%s",
            getattr(trade, "stock_code", "?"),
            getattr(trade, "traded_volume", 0),
            getattr(trade, "traded_price", 0.0),
            getattr(trade, "trade_time", ""),
        )

    def on_order_error(self, order_error):
        logger.error(
            "[推送] 委托失败: %s", getattr(order_error, "error_msg", "unknown")
        )

    def on_cancel_error(self, cancel_error):
        logger.error(
            "[推送] 撤单失败: %s", getattr(cancel_error, "error_msg", "unknown")
        )

    def on_disconnected(self):
        logger.warning("[推送] miniQMT 连接已断开")
        global _g_connected
        _g_connected = False


# ---------------------------------------------------------------------
# 辅助函数
# ---------------------------------------------------------------------
def _make_error(message: str) -> Dict[str, Any]:
    return {"success": False, "error": message}


def _safe_float(val) -> float:
    try:
        return float(val) if val is not None else 0.0
    except (TypeError, ValueError):
        return 0.0


def _safe_int(val) -> int:
    try:
        return int(val) if val is not None else 0
    except (TypeError, ValueError):
        return 0


def _format_datetime(dt) -> str:
    if dt is None:
        return ""
    if isinstance(dt, datetime):
        return dt.strftime("%Y-%m-%d %H:%M:%S")
    return str(dt)


# ---------------------------------------------------------------------
# 连接管理
# ---------------------------------------------------------------------
def do_connect(path: str, session_id: int, account_id: str) -> Dict[str, Any]:
    """执行实际连接逻辑，线程不安全（Flask 默认单线程/多进程，本机调用足够）。"""
    global _g_xt_trader, _g_account, _g_callback, _g_connected, _g_account_id

    if not XTQUANT_AVAILABLE:
        return _make_error("xtquant 库未安装或当前环境非 miniQMT")

    # 如果已有连接，先尝试断开
    if _g_xt_trader is not None and _g_connected:
        try:
            _g_xt_trader.stop()
            logger.info("已断开旧连接")
        except Exception as e:
            logger.warning("断开旧连接时出错: %s", e)

    _g_connected = False
    _g_xt_trader = None
    _g_account = None
    _g_callback = None
    _g_account_id = None

    try:
        trader = XtQuantTrader(path, session_id)
        cb = QmtCallback()
        trader.register_callback(cb)

        start_res = trader.start()
        logger.info("trader.start() 返回: %s", start_res)

        connect_res = trader.connect()
        if connect_res != 0:
            return _make_error(f"miniQMT 连接失败，返回码: {connect_res}")

        acc = StockAccount(account_id)
        subscribe_res = trader.subscribe(acc)
        logger.info("subscribe(%s) 返回: %s", account_id, subscribe_res)

        _g_xt_trader = trader
        _g_account = acc
        _g_callback = cb
        _g_account_id = account_id
        _g_connected = True

        logger.info("miniQMT 连接成功 | path=%s | session_id=%s | account=%s", path, session_id, account_id)
        return {"success": True, "message": "connected"}
    except Exception as e:
        logger.exception("连接 miniQMT 异常")
        return _make_error(str(e))


def auto_connect_from_env():
    """启动时根据环境变量自动连接。"""
    path = os.environ.get("QMT_PATH", "").strip()
    account_id = os.environ.get("QMT_ACCOUNT", "").strip()
    session_id_str = os.environ.get("QMT_SESSION_ID", "123456").strip()

    if not path or not account_id:
        logger.info("环境变量 QMT_PATH / QMT_ACCOUNT 未设置，跳过自动连接")
        return

    try:
        session_id = int(session_id_str)
    except ValueError:
        logger.warning("QMT_SESSION_ID 不是有效整数: %s", session_id_str)
        session_id = 123456

    logger.info("尝试从环境变量自动连接 miniQMT...")
    result = do_connect(path, session_id, account_id)
    if result.get("success"):
        logger.info("自动连接成功")
    else:
        logger.error("自动连接失败: %s", result.get("error"))


# ---------------------------------------------------------------------
# HTTP 接口
# ---------------------------------------------------------------------
@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "ok", "connected": _g_connected})


@app.route("/connect", methods=["POST"])
def connect():
    if not XTQUANT_AVAILABLE:
        return jsonify(_make_error("xtquant 库未安装")), 503

    data = request.get_json(force=True, silent=True) or {}
    path = data.get("path", "").strip()
    session_id = data.get("session_id", 123456)
    account_id = data.get("account_id", "").strip()

    if not path:
        return jsonify(_make_error("缺少 path 参数")), 400
    if not account_id:
        return jsonify(_make_error("缺少 account_id 参数")), 400

    try:
        session_id = int(session_id)
    except (TypeError, ValueError):
        return jsonify(_make_error("session_id 必须是整数")), 400

    result = do_connect(path, session_id, account_id)
    status_code = 200 if result.get("success") else 500
    return jsonify(result), status_code


@app.route("/order", methods=["POST"])
def order():
    if not _g_connected or _g_xt_trader is None:
        return jsonify(_make_error("未连接 miniQMT，请先调用 /connect")), 503

    data = request.get_json(force=True, silent=True) or {}
    stock_code = data.get("stock_code", "").strip()
    order_type_str = data.get("order_type", "").strip().lower()
    volume = data.get("volume", 0)
    price_type_str = data.get("price_type", "").strip().lower()
    price = data.get("price", 0.0)
    strategy_name = data.get("strategy_name", "")
    remark = data.get("remark", "")

    if not stock_code:
        return jsonify(_make_error("缺少 stock_code 参数")), 400
    if order_type_str not in ORDER_TYPE_MAP:
        return jsonify(_make_error(f"order_type 必须是 buy 或 sell，收到: {order_type_str}")), 400
    if price_type_str not in PRICE_TYPE_MAP:
        return jsonify(_make_error(f"price_type 必须是 fix 或 latest，收到: {price_type_str}")), 400

    try:
        volume = int(volume)
        if volume <= 0:
            raise ValueError("volume 必须大于 0")
    except (TypeError, ValueError) as e:
        return jsonify(_make_error(f"volume 参数错误: {e}")), 400

    try:
        price = float(price)
    except (TypeError, ValueError):
        return jsonify(_make_error("price 必须是数字")), 400

    order_type = ORDER_TYPE_MAP[order_type_str]
    price_type = PRICE_TYPE_MAP[price_type_str]

    try:
        order_id = _g_xt_trader.order_stock(
            _g_account,
            stock_code,
            order_type,
            volume,
            price_type,
            price,
            strategy_name,
            remark,
        )
        logger.info(
            "下单成功 %s | %s | 量=%s | 价=%s | price_type=%s | order_id=%s",
            stock_code,
            "买入" if order_type_str == "buy" else "卖出",
            volume,
            price,
            price_type_str,
            order_id,
        )
        return jsonify({"success": True, "order_id": str(order_id) if order_id else ""})
    except Exception as e:
        logger.exception("下单异常")
        return jsonify(_make_error(str(e))), 500


@app.route("/cancel", methods=["POST"])
def cancel():
    if not _g_connected or _g_xt_trader is None:
        return jsonify(_make_error("未连接 miniQMT，请先调用 /connect")), 503

    data = request.get_json(force=True, silent=True) or {}
    order_id = data.get("order_id", "")
    if not order_id:
        return jsonify(_make_error("缺少 order_id 参数")), 400

    try:
        result = _g_xt_trader.cancel_order_stock(_g_account, str(order_id))
        logger.info("撤单请求 %s | 返回=%s", order_id, result)
        return jsonify({"success": True, "result": result})
    except Exception as e:
        logger.exception("撤单异常")
        return jsonify(_make_error(str(e))), 500


@app.route("/positions", methods=["GET"])
def positions():
    if not _g_connected or _g_xt_trader is None:
        return jsonify(_make_error("未连接 miniQMT，请先调用 /connect")), 503

    try:
        raw_positions = _g_xt_trader.query_stock_positions(_g_account)
        result: List[Dict[str, Any]] = []
        for pos in raw_positions or []:
            result.append({
                "stock_code": getattr(pos, "stock_code", ""),
                "volume": _safe_int(getattr(pos, "volume", 0)),
                "avg_price": _safe_float(getattr(pos, "avg_price", 0.0)),
                "open_price": _safe_float(getattr(pos, "open_price", 0.0)),
                "market_value": _safe_float(getattr(pos, "market_value", 0.0)),
            })
        return jsonify({"success": True, "positions": result})
    except Exception as e:
        logger.exception("查询持仓异常")
        return jsonify(_make_error(str(e))), 500


@app.route("/asset", methods=["GET"])
def asset():
    if not _g_connected or _g_xt_trader is None:
        return jsonify(_make_error("未连接 miniQMT，请先调用 /connect")), 503

    try:
        a = _g_xt_trader.query_stock_asset(_g_account)
        return jsonify({
            "success": True,
            "cash": _safe_float(getattr(a, "cash", 0.0)),
            "total_asset": _safe_float(getattr(a, "total_asset", 0.0)),
            "market_value": _safe_float(getattr(a, "market_value", 0.0)),
        })
    except Exception as e:
        logger.exception("查询资产异常")
        return jsonify(_make_error(str(e))), 500


@app.route("/orders", methods=["GET"])
def orders():
    if not _g_connected or _g_xt_trader is None:
        return jsonify(_make_error("未连接 miniQMT，请先调用 /connect")), 503

    try:
        raw_orders = _g_xt_trader.query_stock_orders(_g_account)
        result: List[Dict[str, Any]] = []
        for o in raw_orders or []:
            status_code = _safe_int(getattr(o, "order_status", 255))
            result.append({
                "order_id": str(getattr(o, "order_id", "")),
                "stock_code": getattr(o, "stock_code", ""),
                "order_type": "buy" if getattr(o, "order_type", 0) == 23 else "sell",
                "order_volume": _safe_int(getattr(o, "order_volume", 0)),
                "traded_volume": _safe_int(getattr(o, "traded_volume", 0)),
                "price": _safe_float(getattr(o, "price", 0.0)),
                "order_status": ORDER_STATUS_MAP.get(status_code, "未知"),
                "order_status_code": status_code,
            })
        return jsonify({"success": True, "orders": result})
    except Exception as e:
        logger.exception("查询委托异常")
        return jsonify(_make_error(str(e))), 500


@app.route("/trades", methods=["GET"])
def trades():
    if not _g_connected or _g_xt_trader is None:
        return jsonify(_make_error("未连接 miniQMT，请先调用 /connect")), 503

    try:
        raw_trades = _g_xt_trader.query_stock_trades(_g_account)
        result: List[Dict[str, Any]] = []
        for t in raw_trades or []:
            result.append({
                "order_id": str(getattr(t, "order_id", "")),
                "stock_code": getattr(t, "stock_code", ""),
                "traded_volume": _safe_int(getattr(t, "traded_volume", 0)),
                "traded_price": _safe_float(getattr(t, "traded_price", 0.0)),
                "trade_time": getattr(t, "trade_time", ""),
            })
        return jsonify({"success": True, "trades": result})
    except Exception as e:
        logger.exception("查询成交异常")
        return jsonify(_make_error(str(e))), 500


# ---------------------------------------------------------------------
# 主入口
# ---------------------------------------------------------------------
if __name__ == "__main__":
    host = os.environ.get("QMT_HOST", DEFAULT_HOST).strip()
    port_str = os.environ.get("QMT_PORT", str(DEFAULT_PORT)).strip()
    try:
        port = int(port_str)
    except ValueError:
        logger.warning("QMT_PORT 无效，使用默认端口 %s", DEFAULT_PORT)
        port = DEFAULT_PORT

    auto_connect_from_env()

    logger.info("=" * 60)
    logger.info("miniQMT Sidecar 启动")
    logger.info("监听地址: %s:%s", host, port)
    logger.info("xtquant 可用: %s", XTQUANT_AVAILABLE)
    logger.info("=" * 60)

    # Flask 生产环境警告可忽略，本机 IPC 场景使用单线程足够
    app.run(host=host, port=port, threaded=True)
