#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Samryetha 对外状态页生成器。

statuspage 风格 · 6 语言 · 亮/暗主题 · 确定性故障检测引擎。

检测引擎（纯算法，无 AI）：
- 多源探测器：HTTP/进程/日志/数据库/系统/证书/更新系统，全部基于实测证据
- 滞回机制：连续 N 次异常才立案（防抖），连续 M 次恢复才结案（防闪烁）
- 每条事件带量化证据（次数/百分比/毫秒/天数），可复核、可复现
- 与 AI 诊断互补：秒级出结果、零成本、结论确定
"""

import json
import os
import re
import shutil
import sqlite3
import subprocess
import time
import urllib.request
from datetime import datetime, timedelta, timezone

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
WWW = os.path.join(ROOT, "status", "www")
DATA_DIR = os.path.join(ROOT, "status", "data")
HISTORY_FILE = os.path.join(DATA_DIR, "history.json")
REPO_URL = "https://github.com/Samryetha-Development/Samryetha.git"
DB_PATH = os.path.join(ROOT, "backend", "data", "app.db")
BACKEND_LOG = os.path.join(ROOT, "logs", "backend.log")
FRONTEND_LOG = os.path.join(ROOT, "logs", "frontend.log")
UPDATE_LOG = os.path.join(ROOT, "logs", "update.log")
MARKER = os.path.join(ROOT, "logs", ".last-deployed")
DEV_ROOT = "/opt/Samryetha-dev"
DEV_DB_PATH = os.path.join(DEV_ROOT, "backend", "data", "app.db")
DEV_MARKER = os.path.join(ROOT, "logs", ".last-deployed-dev")
DAYS = 90
LANGS = ["zh", "en", "ja", "fr", "de", "es"]

# ============================ 多语言文案 ============================
STRINGS = {
    "zh": {
        "title": "Samryetha 服务状态",
        "all_ok": "所有系统正常运行", "partial": "部分系统异常", "major": "服务中断",
        "h_components": "系统组件", "h_uptime": "过去 90 天可用率", "h_metrics": "实时指标",
        "h_community": "社区动态", "h_incidents": "事件记录",
        "c_website": "前端网站", "c_api": "后端 API", "c_db": "数据库", "c_update": "自动更新",
        "operational": "正常运行", "outage": "故障", "unknown": "未知",
        "manual": "需人工适配", "pending": "待部署 {sha}",
        "m_online": "当前在线", "m_users": "注册用户", "m_posts": "帖子", "m_replies": "回复",
        "m_requests": "近 5 分钟请求", "m_avg": "平均响应", "m_bemem": "后端内存", "m_femem": "前端内存",
        "m_disk": "磁盘", "m_mem": "内存", "m_load": "负载", "m_hostup": "主机运行",
        "m_codes": "代码缺陷", "m_cwarns": "代码隐患",
        "daily": "按天统计", "today": "今天", "days_ago": "{n} 天前", "collecting": "积累中",
        "nodata": "无数据",
        "empty": "暂无内容", "updated_at": "更新于", "stale": "状态数据已超过 5 分钟未更新",
        "d_website": "samryetha.com · 响应 {ms} ms",
        "d_api": "响应 {ms} ms · 进程运行 {up}",
        "d_db": "SQLite · 进程运行 {up}",
        "d_update": "当前版本 {sha} · {time} 部署",
        "h_dev": "开发环境（development.samryetha.com）",
        "c_dev_website": "Dev 前端", "c_dev_api": "Dev 后端 API", "c_dev_db": "Dev 数据库", "c_dev_update": "Dev 自动更新",
        "d_dev_website": "development.samryetha.com · 响应 {ms} ms",
        "d_dev_api": "响应 {ms} ms · 进程运行 {up}",
        "d_dev_db": "SQLite · 进程运行 {up}",
        "d_dev_update": "版本 {sha} · {time} 部署（数据随更新重置）",
        "replies": "{n} 回复",
        "up_dh": "{d} 天 {h} 小时", "up_hm": "{h} 小时 {m} 分", "up_m": "{m} 分钟",
        "ongoing": "进行中",
        "no_incidents": "90 天内无事件记录",
        "i_code_issues": "代码体检发现 {n} 个新问题：{top}",
        "h_audit": "代码体检明细", "t_sev": "严重级", "t_loc": "位置", "t_rule": "规则", "t_msg": "说明",
        "t_light": "浅色", "t_dark": "深色", "t_system": "跟随系统", "audit_empty": "暂无明细",
        "admin_link": "更新后台",
        "i_core_down": "服务不可用：{comps}",
        "i_api_5xx": "接口 5xx 错误 {n} 次 / {total}（近 5 分钟）",
        "i_be_errors": "后端错误日志 {n} 条（近 5 分钟）{sample}",
        "i_fe_errors": "前端错误日志 {n} 条（近 10 分钟）{sample}",
        "i_crash_restart": "非部署时段进程重启 {delta} 次（{name}）",
        "i_db_integrity": "数据库完整性检查失败：{result}",
        "i_disk": "磁盘使用率 {pct}%",
        "i_mem": "内存使用率 {pct}%",
        "i_load": "系统负载 {load}（{cores} 核）",
        "i_latency": "后端平均响应 {ms} ms",
        "i_cert": "{domain} 证书将于 {days} 天后过期",
        "i_update_stuck": "自动更新 {minutes} 分钟未能完成：{reason}",
        "i_check_gap": "状态检查间隔异常（{gap} 秒），可能有服务重启",
    },
    "en": {
        "title": "Samryetha Status",
        "all_ok": "All Systems Operational", "partial": "Partial Outage", "major": "Major Outage",
        "h_components": "System Components", "h_uptime": "90-Day Uptime", "h_metrics": "Live Metrics",
        "h_community": "Community Activity", "h_incidents": "Incident History",
        "c_website": "Website", "c_api": "API Backend", "c_db": "Database", "c_update": "Auto-Update",
        "operational": "Operational", "outage": "Outage", "unknown": "Unknown",
        "manual": "Manual fix needed", "pending": "Pending deploy {sha}",
        "m_online": "Online now", "m_users": "Registered users", "m_posts": "Posts", "m_replies": "Replies",
        "m_requests": "Requests (5 min)", "m_avg": "Avg response", "m_bemem": "Backend memory", "m_femem": "Frontend memory",
        "m_disk": "Disk", "m_mem": "Memory", "m_load": "Load", "m_hostup": "Host uptime",
        "m_codes": "Code errors", "m_cwarns": "Code warnings",
        "daily": "daily", "today": "Today", "days_ago": "{n} days ago", "collecting": "Collecting…",
        "nodata": "No data",
        "empty": "Nothing here yet", "updated_at": "Updated at", "stale": "Status data is more than 5 minutes old",
        "d_website": "samryetha.com · response {ms} ms",
        "d_api": "response {ms} ms · uptime {up}",
        "d_db": "SQLite · uptime {up}",
        "d_update": "version {sha} · deployed {time}",
        "h_dev": "Development (development.samryetha.com)",
        "c_dev_website": "Dev Website", "c_dev_api": "Dev API Backend", "c_dev_db": "Dev Database", "c_dev_update": "Dev Auto-Update",
        "d_dev_website": "development.samryetha.com · response {ms} ms",
        "d_dev_api": "response {ms} ms · uptime {up}",
        "d_dev_db": "SQLite · uptime {up}",
        "d_dev_update": "version {sha} · deployed {time} (data reset on update)",
        "replies": "{n} replies",
        "up_dh": "{d}d {h}h", "up_hm": "{h}h {m}m", "up_m": "{m}m",
        "ongoing": "Ongoing",
        "no_incidents": "No incidents in the last 90 days",
        "i_code_issues": "Code audit found {n} new issues: {top}",
        "h_audit": "Code Audit Details", "t_sev": "Severity", "t_loc": "Location", "t_rule": "Rule", "t_msg": "Message",
        "t_light": "Light", "t_dark": "Dark", "t_system": "System", "audit_empty": "No findings",
        "admin_link": "Update console",
        "i_core_down": "Service unavailable: {comps}",
        "i_api_5xx": "{n} of {total} requests returned 5xx (5 min)",
        "i_be_errors": "{n} backend error log entries (5 min) {sample}",
        "i_fe_errors": "{n} frontend error log entries (10 min) {sample}",
        "i_crash_restart": "{delta} unexpected process restart(s) outside deploys ({name})",
        "i_db_integrity": "Database integrity check failed: {result}",
        "i_disk": "Disk usage {pct}%",
        "i_mem": "Memory usage {pct}%",
        "i_load": "System load {load} ({cores} cores)",
        "i_latency": "Backend avg response {ms} ms",
        "i_cert": "{domain} certificate expires in {days} days",
        "i_update_stuck": "Auto-update incomplete for {minutes} min: {reason}",
        "i_check_gap": "Abnormal check interval ({gap} s), possible service restart",
    },
    "ja": {
        "title": "Samryetha ステータス",
        "all_ok": "すべてのシステムが正常に稼働中", "partial": "一部のシステムに障害", "major": "大規模な障害",
        "h_components": "システムコンポーネント", "h_uptime": "直近 90 日の稼働率", "h_metrics": "リアルタイム指標",
        "h_community": "コミュニティの最新情報", "h_incidents": "障害履歴",
        "c_website": "ウェブサイト", "c_api": "API バックエンド", "c_db": "データベース", "c_update": "自動アップデート",
        "operational": "正常稼働中", "outage": "障害", "unknown": "不明",
        "manual": "手動対応が必要", "pending": "デプロイ待ち {sha}",
        "m_online": "現在のオンライン", "m_users": "登録ユーザー", "m_posts": "投稿", "m_replies": "返信",
        "m_requests": "リクエスト（5分）", "m_avg": "平均応答", "m_bemem": "バックエンドメモリ", "m_femem": "フロントエンドメモリ",
        "m_disk": "ディスク", "m_mem": "メモリ", "m_load": "負荷", "m_hostup": "ホスト稼働時間",
        "m_codes": "コード欠陥", "m_cwarns": "コード警告",
        "daily": "日別", "today": "今日", "days_ago": "{n}日前", "collecting": "蓄積中…",
        "nodata": "データなし",
        "empty": "まだありません", "updated_at": "更新時刻", "stale": "ステータスデータが 5 分以上更新されていません",
        "d_website": "samryetha.com · 応答 {ms} ms",
        "d_api": "応答 {ms} ms · 稼働 {up}",
        "d_db": "SQLite · 稼働 {up}",
        "d_update": "バージョン {sha} · {time} にデプロイ",
        "h_dev": "開発環境（development.samryetha.com）",
        "c_dev_website": "Dev ウェブサイト", "c_dev_api": "Dev API バックエンド", "c_dev_db": "Dev データベース", "c_dev_update": "Dev 自動アップデート",
        "d_dev_website": "development.samryetha.com · 応答 {ms} ms",
        "d_dev_api": "応答 {ms} ms · 稼働 {up}",
        "d_dev_db": "SQLite · 稼働 {up}",
        "d_dev_update": "バージョン {sha} · {time} にデプロイ（更新時にデータをリセット）",
        "replies": "返信 {n}",
        "up_dh": "{d}日 {h}時間", "up_hm": "{h}時間{m}分", "up_m": "{m}分",
        "ongoing": "継続中",
        "no_incidents": "直近 90 日の障害記録はありません",
        "i_code_issues": "コード検査で {n} 件の新規問題を検出：{top}",
        "h_audit": "コード監査の詳細", "t_sev": "重要度", "t_loc": "場所", "t_rule": "ルール", "t_msg": "説明",
        "t_light": "ライト", "t_dark": "ダーク", "t_system": "システムに従う", "audit_empty": "該当なし",
        "admin_link": "更新コンソール",
        "i_core_down": "サービス利用不可：{comps}",
        "i_api_5xx": "5xx エラー {n} 件 / {total}（直近5分）",
        "i_be_errors": "バックエンドエラーログ {n} 件（直近5分）{sample}",
        "i_fe_errors": "フロントエンドエラーログ {n} 件（直近10分）{sample}",
        "i_crash_restart": "デプロイ時間外のプロセス再起動 {delta} 回（{name}）",
        "i_db_integrity": "データベース整合性チェック失敗：{result}",
        "i_disk": "ディスク使用率 {pct}%",
        "i_mem": "メモリ使用率 {pct}%",
        "i_load": "システム負荷 {load}（{cores} コア）",
        "i_latency": "バックエンド平均応答 {ms} ms",
        "i_cert": "{domain} の証明書は {days} 日後に失効",
        "i_update_stuck": "自動更新が {minutes} 分間未完了：{reason}",
        "i_check_gap": "チェック間隔が異常（{gap} 秒）、再起動の可能性",
    },
    "fr": {
        "title": "État du service Samryetha",
        "all_ok": "Tous les systèmes sont opérationnels", "partial": "Panne partielle", "major": "Panne majeure",
        "h_components": "Composants du système", "h_uptime": "Disponibilité sur 90 jours", "h_metrics": "Métriques en direct",
        "h_community": "Activité de la communauté", "h_incidents": "Historique des incidents",
        "c_website": "Site web", "c_api": "API backend", "c_db": "Base de données", "c_update": "Mise à jour auto",
        "operational": "Opérationnel", "outage": "Panne", "unknown": "Inconnu",
        "manual": "Intervention manuelle requise", "pending": "Déploiement en attente {sha}",
        "m_online": "En ligne", "m_users": "Utilisateurs", "m_posts": "Publications", "m_replies": "Réponses",
        "m_requests": "Requêtes (5 min)", "m_avg": "Réponse moy.", "m_bemem": "Mémoire backend", "m_femem": "Mémoire frontend",
        "m_disk": "Disque", "m_mem": "Mémoire", "m_load": "Charge", "m_hostup": "Uptime hôte",
        "m_codes": "Erreurs code", "m_cwarns": "Avertissements code",
        "daily": "par jour", "today": "Aujourd'hui", "days_ago": "il y a {n} jours", "collecting": "Collecte…",
        "nodata": "Aucune donnée",
        "empty": "Rien pour le moment", "updated_at": "Mis à jour à", "stale": "Les données datent de plus de 5 minutes",
        "d_website": "samryetha.com · réponse {ms} ms",
        "d_api": "réponse {ms} ms · en marche {up}",
        "d_db": "SQLite · en marche {up}",
        "d_update": "version {sha} · déployée {time}",
        "h_dev": "Développement (development.samryetha.com)",
        "c_dev_website": "Site dev", "c_dev_api": "API dev", "c_dev_db": "Base de données dev", "c_dev_update": "MàJ auto dev",
        "d_dev_website": "development.samryetha.com · réponse {ms} ms",
        "d_dev_api": "réponse {ms} ms · en marche {up}",
        "d_dev_db": "SQLite · en marche {up}",
        "d_dev_update": "version {sha} · déployée {time} (données réinitialisées)",
        "replies": "{n} réponses",
        "up_dh": "{d} j {h} h", "up_hm": "{h} h {m} min", "up_m": "{m} min",
        "ongoing": "En cours",
        "no_incidents": "Aucun incident au cours des 90 derniers jours",
        "i_code_issues": "L'audit du code a détecté {n} nouveaux problèmes : {top}",
        "h_audit": "Détails de l'audit du code", "t_sev": "Gravité", "t_loc": "Emplacement", "t_rule": "Règle", "t_msg": "Message",
        "t_light": "Clair", "t_dark": "Sombre", "t_system": "Système", "audit_empty": "Aucun résultat",
        "admin_link": "Console de mise à jour",
        "i_core_down": "Service indisponible : {comps}",
        "i_api_5xx": "{n} réponses 5xx sur {total} (5 min)",
        "i_be_errors": "{n} entrées d'erreur backend (5 min) {sample}",
        "i_fe_errors": "{n} entrées d'erreur frontend (10 min) {sample}",
        "i_crash_restart": "{delta} redémarrage(s) hors déploiement ({name})",
        "i_db_integrity": "Échec du contrôle d'intégrité : {result}",
        "i_disk": "Utilisation du disque {pct}%",
        "i_mem": "Utilisation mémoire {pct}%",
        "i_load": "Charge système {load} ({cores} cœurs)",
        "i_latency": "Réponse backend moyenne {ms} ms",
        "i_cert": "Le certificat {domain} expire dans {days} jours",
        "i_update_stuck": "Mise à jour incomplète depuis {minutes} min : {reason}",
        "i_check_gap": "Intervalle de vérification anormal ({gap} s)",
    },
    "de": {
        "title": "Samryetha Servicestatus",
        "all_ok": "Alle Systeme sind funktionsfähig", "partial": "Teilausfall", "major": "Schwerer Ausfall",
        "h_components": "Systemkomponenten", "h_uptime": "Verfügbarkeit (90 Tage)", "h_metrics": "Live-Metriken",
        "h_community": "Community-Aktivität", "h_incidents": "Vorfallverlauf",
        "c_website": "Webseite", "c_api": "API-Backend", "c_db": "Datenbank", "c_update": "Auto-Update",
        "operational": "Funktionsfähig", "outage": "Ausfall", "unknown": "Unbekannt",
        "manual": "Manuelle Anpassung nötig", "pending": "Bereitstellung ausstehend {sha}",
        "m_online": "Jetzt online", "m_users": "Registrierte Nutzer", "m_posts": "Beiträge", "m_replies": "Antworten",
        "m_requests": "Anfragen (5 Min.)", "m_avg": "Ø Antwort", "m_bemem": "Backend-Speicher", "m_femem": "Frontend-Speicher",
        "m_disk": "Festplatte", "m_mem": "RAM", "m_load": "Load", "m_hostup": "Host-Laufzeit",
        "m_codes": "Code-Fehler", "m_cwarns": "Code-Warnungen",
        "daily": "täglich", "today": "Heute", "days_ago": "vor {n} Tagen", "collecting": "Wird erfasst…",
        "nodata": "Keine Daten",
        "empty": "Noch nichts vorhanden", "updated_at": "Aktualisiert am", "stale": "Statusdaten sind älter als 5 Minuten",
        "d_website": "samryetha.com · Antwort {ms} ms",
        "d_api": "Antwort {ms} ms · Laufzeit {up}",
        "d_db": "SQLite · Laufzeit {up}",
        "d_update": "Version {sha} · bereitgestellt {time}",
        "h_dev": "Entwicklung (development.samryetha.com)",
        "c_dev_website": "Dev-Webseite", "c_dev_api": "Dev-API-Backend", "c_dev_db": "Dev-Datenbank", "c_dev_update": "Dev-Auto-Update",
        "d_dev_website": "development.samryetha.com · Antwort {ms} ms",
        "d_dev_api": "Antwort {ms} ms · Laufzeit {up}",
        "d_dev_db": "SQLite · Laufzeit {up}",
        "d_dev_update": "Version {sha} · bereitgestellt {time} (Daten bei Update zurückgesetzt)",
        "replies": "{n} Antworten",
        "up_dh": "{d} T {h} Std", "up_hm": "{h} Std. {m} Min.", "up_m": "{m} Min.",
        "ongoing": "Laufend",
        "no_incidents": "Keine Vorfälle in den letzten 90 Tagen",
        "i_code_issues": "Code-Audit fand {n} neue Probleme: {top}",
        "h_audit": "Code-Audit-Details", "t_sev": "Schweregrad", "t_loc": "Ort", "t_rule": "Regel", "t_msg": "Meldung",
        "t_light": "Hell", "t_dark": "Dunkel", "t_system": "System", "audit_empty": "Keine Befunde",
        "admin_link": "Update-Konsole",
        "i_core_down": "Dienst nicht verfügbar: {comps}",
        "i_api_5xx": "{n} von {total} Anfragen mit 5xx (5 Min.)",
        "i_be_errors": "{n} Backend-Fehlerlogeinträge (5 Min.) {sample}",
        "i_fe_errors": "{n} Frontend-Fehlerlogeinträge (10 Min.) {sample}",
        "i_crash_restart": "{delta} Neustart(s) außerhalb von Bereitstellungen ({name})",
        "i_db_integrity": "Integritätsprüfung fehlgeschlagen: {result}",
        "i_disk": "Festplattennutzung {pct}%",
        "i_mem": "RAM-Nutzung {pct}%",
        "i_load": "Systemlast {load} ({cores} Kerne)",
        "i_latency": "Ø Backend-Antwort {ms} ms",
        "i_cert": "{domain}-Zertifikat läuft in {days} Tagen ab",
        "i_update_stuck": "Auto-Update seit {minutes} Min. unvollständig: {reason}",
        "i_check_gap": "Anomaler Prüfintervall ({gap} s), möglicher Neustart",
    },
    "es": {
        "title": "Estado del servicio de Samryetha",
        "all_ok": "Todos los sistemas operativos", "partial": "Interrupción parcial", "major": "Interrupción grave",
        "h_components": "Componentes del sistema", "h_uptime": "Disponibilidad de 90 días", "h_metrics": "Métricas en vivo",
        "h_community": "Actividad de la comunidad", "h_incidents": "Historial de incidentes",
        "c_website": "Sitio web", "c_api": "Backend de la API", "c_db": "Base de datos", "c_update": "Actualización automática",
        "operational": "Operativo", "outage": "Caída", "unknown": "Desconocido",
        "manual": "Requiere ajuste manual", "pending": "Despliegue pendiente {sha}",
        "m_online": "En línea", "m_users": "Usuarios registrados", "m_posts": "Publicaciones", "m_replies": "Respuestas",
        "m_requests": "Peticiones (5 min)", "m_avg": "Respuesta media", "m_bemem": "Memoria backend", "m_femem": "Memoria frontend",
        "m_disk": "Disco", "m_mem": "Memoria", "m_load": "Carga", "m_hostup": "Uptime del host",
        "m_codes": "Errores de código", "m_cwarns": "Avisos de código",
        "daily": "por día", "today": "Hoy", "days_ago": "hace {n} días", "collecting": "Recopilando…",
        "nodata": "Sin datos",
        "empty": "Aún no hay contenido", "updated_at": "Actualizado el", "stale": "Los datos tienen más de 5 minutos",
        "d_website": "samryetha.com · respuesta {ms} ms",
        "d_api": "respuesta {ms} ms · en marcha {up}",
        "d_db": "SQLite · en marcha {up}",
        "d_update": "versión {sha} · desplegada {time}",
        "h_dev": "Desarrollo (development.samryetha.com)",
        "c_dev_website": "Sitio dev", "c_dev_api": "API dev", "c_dev_db": "Base de datos dev", "c_dev_update": "Actualización dev",
        "d_dev_website": "development.samryetha.com · respuesta {ms} ms",
        "d_dev_api": "respuesta {ms} ms · en marcha {up}",
        "d_dev_db": "SQLite · en marcha {up}",
        "d_dev_update": "versión {sha} · desplegada {time} (datos restablecidos)",
        "replies": "{n} respuestas",
        "up_dh": "{d} d {h} h", "up_hm": "{h} h {m} min", "up_m": "{m} min",
        "ongoing": "En curso",
        "no_incidents": "Sin incidentes en los últimos 90 días",
        "i_code_issues": "La auditoría de código encontró {n} problemas nuevos: {top}",
        "h_audit": "Detalles de la auditoría de código", "t_sev": "Gravedad", "t_loc": "Ubicación", "t_rule": "Regla", "t_msg": "Mensaje",
        "t_light": "Claro", "t_dark": "Oscuro", "t_system": "Sistema", "audit_empty": "Sin hallazgos",
        "admin_link": "Consola de actualización",
        "i_core_down": "Servicio no disponible: {comps}",
        "i_api_5xx": "{n} de {total} peticiones con 5xx (5 min)",
        "i_be_errors": "{n} entradas de error del backend (5 min) {sample}",
        "i_fe_errors": "{n} entradas de error del frontend (10 min) {sample}",
        "i_crash_restart": "{delta} reinicio(s) fuera de despliegues ({name})",
        "i_db_integrity": "Fallo de verificación de integridad: {result}",
        "i_disk": "Uso de disco {pct}%",
        "i_mem": "Uso de memoria {pct}%",
        "i_load": "Carga del sistema {load} ({cores} núcleos)",
        "i_latency": "Respuesta media del backend {ms} ms",
        "i_cert": "El certificado de {domain} expira en {days} días",
        "i_update_stuck": "Actualización incompleta desde hace {minutes} min: {reason}",
        "i_check_gap": "Intervalo de verificación anómalo ({gap} s)",
    },
}

LANG_NAMES = {"zh": "中文", "en": "English", "ja": "日本語", "fr": "Français", "de": "Deutsch", "es": "Español"}


# ============================ 数据采集 ============================
def http_json(url, timeout=5):
    start = time.perf_counter()
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "samryetha-status/1.0"})
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read(262144)
            ms = int((time.perf_counter() - start) * 1000)
            try:
                data = json.loads(body.decode("utf-8", "replace"))
            except Exception:
                data = None
            return {"ok": 200 <= resp.status < 400, "ms": ms, "data": data}
    except Exception:
        return {"ok": False, "ms": int((time.perf_counter() - start) * 1000), "data": None}


def pm2_apps():
    try:
        out = subprocess.run(["pm2", "jlist"], capture_output=True, text=True, timeout=20)
        return [a for a in json.loads(out.stdout) if a.get("name")]
    except Exception:
        return []


def db_counts():
    try:
        con = sqlite3.connect(f"file:{DB_PATH}?mode=ro", uri=True, timeout=3)
        cur = con.cursor()
        def cnt(t):
            try:
                return cur.execute(f"SELECT COUNT(*) FROM {t}").fetchone()[0]
            except Exception:
                return None
        r = {t: cnt(t) for t in ("users", "discussions", "replies", "boards")}
        con.close()
        return r
    except Exception:
        return {}


def db_integrity():
    try:
        con = sqlite3.connect(f"file:{DB_PATH}?mode=ro", uri=True, timeout=5)
        row = con.execute("PRAGMA quick_check").fetchone()
        con.close()
        return (row[0] if row else "unknown")
    except Exception as e:
        return f"error: {e}"


def parse_backend_log(seconds=300):
    """一次解析，供流量指标与错误检测共用。"""
    out = {"rpm": 0, "avg_ms": None, "n5xx": 0, "total": 0, "err_n": 0, "err_sample": None}
    try:
        r = subprocess.run(["tail", "-n", "4000", BACKEND_LOG], capture_output=True, text=True, timeout=10)
        cutoff = time.time() * 1000 - seconds * 1000
        rt_sum = rt_n = 0
        for line in r.stdout.splitlines():
            try:
                d = json.loads(line)
            except Exception:
                continue
            t = d.get("time", 0)
            if not isinstance(t, (int, float)) or t < cutoff:
                continue
            msg = d.get("msg", "")
            if msg == "incoming request":
                out["rpm"] += 1
            elif msg == "request completed":
                out["total"] += 1
                code = d.get("res", {}).get("statusCode", 0)
                if isinstance(code, int) and code >= 500:
                    out["n5xx"] += 1
                rt = d.get("responseTime")
                if isinstance(rt, (int, float)):
                    rt_sum += rt
                    rt_n += 1
            elif d.get("level", 0) >= 50:
                out["err_n"] += 1
                if not out["err_sample"]:
                    out["err_sample"] = str(msg)[:120]
        if rt_n:
            out["avg_ms"] = round(rt_sum / rt_n, 1)
    except Exception:
        pass
    return out


def parse_frontend_log(minutes=10):
    out = {"err_n": 0, "err_sample": None}
    try:
        r = subprocess.run(["tail", "-n", "300", FRONTEND_LOG], capture_output=True, text=True, timeout=10)
        cutoff = datetime.now() - timedelta(minutes=minutes)
        for line in r.stdout.splitlines():
            if "Error" not in line:
                continue
            try:
                ts = datetime.fromisoformat(line[:19])
            except Exception:
                continue
            if ts < cutoff:
                continue
            out["err_n"] += 1
            if not out["err_sample"]:
                out["err_sample"] = line[line.find("Error"):][:120]
    except Exception:
        pass
    return out


def cert_days_left(domain):
    try:
        r = subprocess.run(["openssl", "s_client", "-connect", "127.0.0.1:443", "-servername", domain],
                           input="", capture_output=True, text=True, timeout=10)
        m = re.search(r"notAfter=(.+)", r.stdout)
        if not m:
            return None
        dt = datetime.strptime(m.group(1).strip(), "%b %d %H:%M:%S %Y %Z").replace(tzinfo=timezone.utc)
        return (dt - datetime.now(timezone.utc)).days
    except Exception:
        return None


def remote_sha(branch="main"):
    try:
        out = subprocess.run(["git", "ls-remote", REPO_URL, f"refs/heads/{branch}"],
                             capture_output=True, text=True, timeout=8)
        s = out.stdout.strip()
        return s.split()[0][:7] if s else None
    except Exception:
        return None


def deployed_info(marker=MARKER):
    try:
        return open(marker, encoding="utf-8").read().strip()[:7], os.path.getmtime(marker)
    except Exception:
        return None, None


def cz_status():
    try:
        out = subprocess.run(["python3", os.path.join(ROOT, "customizations", "apply.py"), "--check"],
                             capture_output=True, text=True, timeout=20)
        lines = out.stdout.splitlines()
        fails = sum(1 for l in lines if l.startswith("FAIL"))
        total = sum(1 for l in lines if l[:3] in ("OK ", "FAI", "MIS"))
        return {"ok": fails == 0, "fails": fails, "total": total}
    except Exception:
        return None


def host_stats():
    du = shutil.disk_usage("/")
    mem_total = mem_avail = 0
    try:
        for line in open("/proc/meminfo", encoding="utf-8"):
            if line.startswith("MemTotal:"):
                mem_total = int(line.split()[1])
            elif line.startswith("MemAvailable:"):
                mem_avail = int(line.split()[1])
    except Exception:
        pass
    try:
        up_s = int(float(open("/proc/uptime").read().split()[0]))
    except Exception:
        up_s = 0
    return {"disk_pct": int(du.used / du.total * 100),
            "mem_pct": int((mem_total - mem_avail) / mem_total * 100) if mem_total else 0,
            "load1": round(os.getloadavg()[0], 2) if hasattr(os, "getloadavg") else 0,
            "cores": os.cpu_count() or 1,
            "host_uptime_s": up_s}


def fmt_uptime(seconds, lang):
    t = STRINGS[lang]
    if not seconds or seconds < 0:
        return t["unknown"]
    d, rem = divmod(int(seconds), 86400)
    h, rem = divmod(rem, 3600)
    m = rem // 60
    if d:
        return t["up_dh"].format(d=d, h=h)
    if h:
        return t["up_hm"].format(h=h, m=m)
    return t["up_m"].format(m=m)


def fmt_time_ms(ms_epoch):
    try:
        return datetime.fromtimestamp(ms_epoch / 1000).strftime("%m-%d %H:%M")
    except Exception:
        return ""


# ============================ 历史与可用率 ============================
def load_history():
    try:
        with open(HISTORY_FILE, encoding="utf-8") as f:
            h = json.load(f)
    except Exception:
        h = {}
    h.setdefault("days", {})
    h.setdefault("incident", None)
    h.setdefault("incidents", [])
    h.setdefault("det", {"state": {}, "first_pending": None, "last_run": None, "last_restarts": None})
    return h


def save_history(h):
    os.makedirs(DATA_DIR, exist_ok=True)
    tmp = HISTORY_FILE + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(h, f, ensure_ascii=False)
    os.replace(tmp, HISTORY_FILE)


def uptime_series(h):
    series = []
    total_ok = total_all = 0
    for i in range(DAYS - 1, -1, -1):
        day = (datetime.now() - timedelta(days=i)).strftime("%Y-%m-%d")
        rec = h.get("days", {}).get(day)
        if rec and (rec["ok"] + rec["down"]) > 0:
            pct = rec["ok"] / (rec["ok"] + rec["down"]) * 100
            total_ok += rec["ok"]
            total_all += rec["ok"] + rec["down"]
            series.append((day, pct))
        else:
            series.append((day, None))
    overall = round(total_ok / total_all * 100, 2) if total_all else None
    return series, overall


# ============================ 检测引擎（纯算法） ============================
# 每个探测器: id / 基础严重级 / 连续几次异常才立案 / 连续几次恢复才结案 / evaluate(d) -> (failing, evidence)
# 事件 = 确定性规则 + 滞回 + 量化证据。无猜测、无 AI。
def detectors(d):
    traf = d["traffic"]
    felog = d["fe_log"]
    host = d["host"]
    dep_time = d["deployed"]["time"]
    now = time.time()
    deploy_recent = bool(dep_time and (now - dep_time) < 300)

    out = []

    comps_down = []
    if not d["frontend"]["ok"]:
        comps_down.append("fe")
    if not d["backend"]["ok"]:
        comps_down.append("be")
    if d["db"] == "error":
        comps_down.append("db")
    if d["processes"]["backend"]["online"] < 1:
        comps_down.append("pm2be")
    if d["processes"]["frontend"]["online"] < 2:
        comps_down.append("pm2fe")
    out.append({"id": "core_down", "sev": "crit", "open_after": 2, "close_after": 3,
                "fail": bool(comps_down), "evidence": {"comps": ",".join(comps_down)}})

    rate5 = (traf["n5xx"] / traf["total"]) if traf["total"] else 0
    out.append({"id": "api_5xx", "sev": "err", "open_after": 2, "close_after": 5,
                "fail": traf["n5xx"] >= 5 and rate5 >= 0.02,
                "evidence": {"n": traf["n5xx"], "total": traf["total"]}})

    out.append({"id": "be_errors", "sev": "err", "open_after": 2, "close_after": 5,
                "fail": traf["err_n"] >= 3,
                "evidence": {"n": traf["err_n"],
                             "sample": ("· " + traf["err_sample"]) if traf["err_sample"] else ""}})

    out.append({"id": "fe_errors", "sev": "warn", "open_after": 2, "close_after": 5,
                "fail": felog["err_n"] >= 3,
                "evidence": {"n": felog["err_n"],
                             "sample": ("· " + felog["err_sample"]) if felog["err_sample"] else ""}})

    restarts_now = d["processes"]["backend"].get("restarts", 0) or 0
    fe_restarts = d["processes"]["frontend"].get("restarts", 0) or 0
    out.append({"id": "crash_restart", "sev": "err", "open_after": 1, "close_after": 2,
                "fail": False, "evidence": {}})  # 由 run_detectors 用状态差值填充
    d["_restarts_total"] = int(restarts_now) + int(fe_restarts)
    d["_deploy_recent"] = deploy_recent

    out.append({"id": "db_integrity", "sev": "crit", "open_after": 1, "close_after": 2,
                "fail": d["db_check"] != "ok", "evidence": {"result": d["db_check"]}})

    out.append({"id": "disk", "sev": "warn", "open_after": 3, "close_after": 5,
                "fail": host["disk_pct"] >= 80, "evidence": {"pct": host["disk_pct"]},
                "sev_fn": lambda ev: "err" if ev.get("pct", 0) >= 90 else "warn"})

    out.append({"id": "mem", "sev": "warn", "open_after": 3, "close_after": 5,
                "fail": host["mem_pct"] >= 90, "evidence": {"pct": host["mem_pct"]}})

    out.append({"id": "load", "sev": "warn", "open_after": 3, "close_after": 5,
                "fail": host["load1"] > host["cores"] * 2,
                "evidence": {"load": host["load1"], "cores": host["cores"]}})

    out.append({"id": "latency", "sev": "warn", "open_after": 3, "close_after": 5,
                "fail": traf["avg_ms"] is not None and traf["avg_ms"] > 2000,
                "evidence": {"ms": traf["avg_ms"] if traf["avg_ms"] is not None else "—"}})

    cert_bad = []
    for dom in ("samryetha.com", "status.samryetha.com"):
        days = cert_days_left(dom)
        if days is not None and days < 15:
            cert_bad.append((dom, days))
    out.append({"id": "cert", "sev": "warn", "open_after": 1, "close_after": 1,
                "fail": bool(cert_bad),
                "evidence": {"domain": cert_bad[0][0], "days": cert_bad[0][1]} if cert_bad else {},
                "sev_fn": lambda ev: "err" if ev.get("days", 99) < 5 else "warn"})

    return out


def run_detectors(h, d):
    """执行检测引擎：滞回立案/结案，事件写入 h["incidents"]。返回是否有未结严重事件。"""
    det = h["det"]
    now = time.time()

    # --- 特殊状态机 1: 进程非部署期重启（用累计重启数差值判断）---
    total = d.get("_restarts_total", 0)
    last = det.get("last_restarts")
    delta = max(0, total - last) if isinstance(last, int) else 0
    det["last_restarts"] = total
    restart_fail = delta > 0 and not d.get("_deploy_recent", False)
    specs = detectors(d)
    for s in specs:
        if s["id"] == "crash_restart":
            s["fail"] = restart_fail
            s["evidence"] = {"delta": delta, "name": "pm2"}
    if d["pending"]:
        if det.get("first_pending") is None:
            det["first_pending"] = now
        pending_minutes = int((now - det["first_pending"]) / 60)
    else:
        det["first_pending"] = None
        pending_minutes = 0
    last_attempt_failed = False
    fail_reason = ""
    try:
        tail = subprocess.run(["tail", "-n", "40", UPDATE_LOG], capture_output=True, text=True, timeout=5).stdout
        for line in reversed(tail.splitlines()):
            if "[ok] 更新完成" in line:
                break
            m = re.search(r"\[fail\]\s*(.+)$", line)
            if m:
                last_attempt_failed = True
                fail_reason = m.group(1)[:60]
                break
    except Exception:
        pass
    for s in specs:
        if s["id"] == "update_stuck_placeholder":
            pass
    specs.append({"id": "update_stuck", "sev": "err", "open_after": 1, "close_after": 1,
                  "fail": bool(d["pending"] and pending_minutes >= 30 and last_attempt_failed),
                  "evidence": {"minutes": pending_minutes, "reason": fail_reason or "unknown"}})

    # --- 特殊状态机 3: 检查间隔异常（检测主机冻结/重启）---
    last_run = det.get("last_run")
    gap = int(now - last_run) if last_run else 0
    specs.append({"id": "check_gap", "sev": "warn", "open_after": 1, "close_after": 1,
                  "fail": bool(last_run and gap > 300), "evidence": {"gap": gap}})
    det["last_run"] = now


    # 代码体检不再进入事件记录：页面下方已有独立的“代码体检明细/代码缺陷”区块，
    # 反复出现的代码问题会把可用性事件淹没。仅保留服务可用性与基础设施类事件。
    # 同时清除历史遗留的 code_issues 事件与状态，避免旧条目长期挂起。
    if any(i.get("kind") == "code_issues" for i in h["incidents"]):
        h["incidents"] = [i for i in h["incidents"] if i.get("kind") != "code_issues"]
    det["state"].pop("code_issues", None)

    # --- 通用滞回执行 ---
    for spec in specs:
        st = det["state"].setdefault(spec["id"], {"fails": 0, "oks": 0})
        if spec["fail"]:
            st["fails"] += 1
            st["oks"] = 0
        else:
            st["oks"] += 1
            st["fails"] = 0

        open_inc = next((i for i in h["incidents"] if i.get("kind") == spec["id"] and i.get("open")), None)
        if spec["fail"] and st["fails"] >= spec["open_after"] and not open_inc:
            ev = spec["evidence"]
            inc = {"kind": spec["id"], "sev": spec["sev"],
                   "start": now, "end": None, "duration_s": None,
                   "open": True, "evidence": ev}
            if spec.get("sev_fn"):
                try:
                    inc["sev"] = spec["sev_fn"](ev)
                except Exception:
                    pass
            h["incidents"].insert(0, inc)
            h["incidents"] = h["incidents"][:50]
        elif spec["fail"] and open_inc:
            open_inc["evidence"] = spec["evidence"]
            if spec.get("sev_fn"):
                try:
                    open_inc["sev"] = spec["sev_fn"](spec["evidence"])
                except Exception:
                    pass
            else:
                open_inc["sev"] = spec["sev"]
        elif not spec["fail"] and open_inc and st["oks"] >= spec["close_after"]:
            open_inc["end"] = now
            open_inc["duration_s"] = max(1, int(now - open_inc["start"]))
            open_inc["open"] = False

    h["incidents"] = h["incidents"][:50]
    return any(i.get("open") and i.get("sev") in ("crit", "err") for i in h["incidents"])



def read_code_report():
    try:
        with open(os.path.join(ROOT, "status", "data", "code-report.json"), encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return None

def collect():
    fe = http_json("http://127.0.0.1:3000/")
    be = http_json("http://127.0.0.1:3001/api/health")
    presence = http_json("http://127.0.0.1:3001/api/presence", timeout=4)
    discussions = http_json("http://127.0.0.1:3001/api/discussions?feed=latest&limit=3", timeout=4)

    # dev 环境（development.samryetha.com，独立于主站显示，不影响主站可用率）
    dev_fe = http_json("http://127.0.0.1:3010/")
    dev_be = http_json("http://127.0.0.1:3011/api/health")

    # 新版 Python 后端 /api/health 只返回 status（不暴露 db/uptime，见 routers/health.py），
    # 因此不再从 health 取；数据库状态用本地 SQLite 实测，运行时长用后端进程 uptime 兜底。
    def health_field(res, key):
        if res["ok"] and isinstance(res["data"], dict):
            return res["data"].get(key)
        return None
    uptime = health_field(be, "uptime")
    dev_uptime = health_field(dev_be, "uptime")

    apps = pm2_apps()
    def stat(name):
        matched = [a for a in apps if a.get("name") == name]
        if not matched:
            return {"online": 0, "mem_mb": None, "uptime": None, "restarts": 0}
        env = matched[0].get("pm2_env", {})
        mem = (matched[0].get("monit") or {}).get("memory")
        return {"online": sum(1 for a in matched if a.get("pm2_env", {}).get("status") == "online"),
                "mem_mb": round(mem / 1048576) if isinstance(mem, (int, float)) else None,
                "uptime": (time.time() * 1000 - env.get("pm_uptime", 0)) / 1000 if env.get("pm_uptime") else None,
                "restarts": env.get("restart_time", 0)}
    be_p = stat("samryetha-backend")
    fe_p = stat("samryetha-frontend")
    dev_be_p = stat("samryetha-dev-backend")
    dev_fe_p = stat("samryetha-dev-frontend")

    # 数据库状态来自本地实测：主库跑 SQLite quick_check，dev 库每次由主站同步、以后端进程在线推断。
    db_check = db_integrity()
    db = "ok" if db_check == "ok" else "error"
    if uptime is None:
        uptime = be_p["uptime"]
    dev_db = "ok" if (dev_be["ok"] and dev_be_p["online"] >= 1) else "?"
    if dev_uptime is None:
        dev_uptime = dev_be_p["uptime"]

    latest = []
    if discussions["ok"] and isinstance(discussions["data"], dict):
        for it in (discussions["data"].get("items") or [])[:3]:
            title = (it.get("title") or "").strip()
            if len(title) > 24:
                title = title[:24] + "…"
            author = ((it.get("author") or {}).get("displayName")
                      or (it.get("author") or {}).get("username") or "")
            latest.append({"title": title, "author": author,
                           "replies": it.get("replyCount", 0), "at": fmt_time_ms(it.get("createdAt"))})

    online = 0
    if presence["ok"] and isinstance(presence["data"], dict):
        online = presence["data"].get("onlineCount", 0)

    dep_sha, dep_time = deployed_info()
    remote = remote_sha("main")
    dev_dep_sha, dev_dep_time = deployed_info(DEV_MARKER)
    dev_remote = remote_sha("dev")

    # 人工配置提醒（update.sh 写入 status/data/config-notice.json，如 SMTP 未接线）
    config_notice = None
    try:
        with open(os.path.join(DATA_DIR, "config-notice.json"), encoding="utf-8") as nf:
            config_notice = json.load(nf)
    except Exception:
        config_notice = None
    return {
        "checked_at": datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
        "checked_ts": time.time(),
        "frontend": {"ok": fe["ok"], "ms": fe["ms"]},
        "backend": {"ok": be["ok"], "ms": be["ms"], "uptime_s": uptime},
        "db": db, "db_check": db_check,
        "processes": {"backend": be_p, "frontend": fe_p},
        "community": {**db_counts(), "online": online, "latest": latest},
        "traffic": parse_backend_log(),
        "fe_log": parse_frontend_log(),
        "deployed": {"sha": dep_sha, "time": dep_time},
        "remote": remote,
        "pending": bool(remote and dep_sha and remote != dep_sha),
        "config_notice": config_notice,
        "customizations": cz_status(),
        "code_report": read_code_report(),
        "host": host_stats(),
        "dev": {
            "frontend": {"ok": dev_fe["ok"], "ms": dev_fe["ms"]},
            "backend": {"ok": dev_be["ok"], "ms": dev_be["ms"], "uptime_s": dev_uptime},
            "db": dev_db,
            "processes": {"backend": dev_be_p, "frontend": dev_fe_p},
            "deployed": {"sha": dev_dep_sha, "time": dev_dep_time},
            "remote": dev_remote,
            "pending": bool(dev_remote and dev_dep_sha and dev_remote != dev_dep_sha),
        },
        "_core_ok": fe["ok"] and be["ok"] and db in ("ok", "?") and be_p["online"] >= 1 and fe_p["online"] >= 2,
        "_dev_ok": (dev_fe["ok"] and dev_be["ok"] and dev_db in ("ok", "?")
                    and dev_be_p["online"] >= 1 and dev_fe_p["online"] >= 1),
    }


# ============================ 渲染 ============================
def esc(s):
    return str(s).replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")



ICONS = {
    "sun": '<circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>',
    "moon": '<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/>',
    "monitor": '<rect width="20" height="14" x="2" y="3" rx="2"/><line x1="8" x2="16" y1="21" y2="21"/><line x1="12" x2="12" y1="17" y2="21"/>',
    "languages": '<path d="m5 8 6 6"/><path d="m4 14 6-6 2-3"/><path d="M2 5h12"/><path d="M7 2h1"/><path d="m22 22-5-10-5 10"/><path d="M14 18h6"/>',
    "sliders": '<line x1="4" x2="4" y1="21" y2="14"/><line x1="4" x2="4" y1="10" y2="3"/><line x1="12" x2="12" y1="21" y2="12"/><line x1="12" x2="12" y1="8" y2="3"/><line x1="20" x2="20" y1="21" y2="16"/><line x1="20" x2="20" y1="12" y2="3"/><line x1="2" x2="6" y1="14" y2="14"/><line x1="10" x2="14" y1="8" y2="8"/><line x1="18" x2="22" y1="16" y2="16"/>',
}

def svg(name, cls=""):
    c = f' class="{cls}"' if cls else ""
    return (f'<svg{c} xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" '
            f'fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" '
            f'stroke-linejoin="round" aria-hidden="true">{ICONS[name]}</svg>')

def lang_switcher(cur):
    items = []
    for lang in LANGS:
        name = LANG_NAMES[lang]
        if lang == cur:
            items.append(f'<li class="cur">{name}</li>')
        else:
            href = "/" if lang == "zh" else f"/{lang}/"
            items.append(f'<li data-lang="{lang}"><a href="{href}">{name}</a></li>')
    return (f'<div class="dd" id="langdd">'
            f'<button class="iconbtn" title="Language" aria-haspopup="menu" '
            f'onclick="event.stopPropagation();document.getElementById(\'langdd\').classList.toggle(\'open\')">{svg("languages")}</button>'
            f'<ul class="ddmenu">{"".join(items)}</ul></div>')


def render_incidents(h, lang):
    t = STRINGS[lang]
    incs = h.get("incidents", [])
    if not incs:
        return f'<div class="empty">{t["no_incidents"]}</div>'
    open_first = sorted(incs, key=lambda i: (not i.get("open"), -i.get("start", 0)))
    comp_names = {"fe": t["c_website"], "be": t["c_api"], "db": t["c_db"],
                  "pm2be": t["c_api"], "pm2fe": t["c_website"]}
    rows = ""
    shown = 0
    for inc in open_first:
        if shown >= 8:
            break
        shown += 1
        ev = inc.get("evidence", {})
        try:
            if inc["kind"] == "core_down":
                comps = ", ".join(comp_names.get(c, c) for c in str(ev.get("comps", "")).split(",") if c)
                text = t["i_core_down"].format(comps=comps or "?")
            else:
                text = t[f'i_{inc["kind"]}'].format(**ev)
        except Exception:
            text = inc["kind"]
        s = datetime.fromtimestamp(inc["start"]).strftime("%m-%d %H:%M")
        dur = inc.get("duration_s")
        if inc.get("open"):
            dur_s = fmt_uptime(time.time() - inc["start"], lang)
            badge = f'<span class="pill">{t["ongoing"]}</span>'
            dotcls = "bad" if inc.get("sev") in ("crit", "err") else "warn"
        else:
            dur_s = fmt_uptime(dur, lang) if dur else "—"
            badge = ""
            dotcls = "bad" if inc.get("sev") in ("crit", "err") else "warn"
        rows += (f'<div class="inc"><span class="inc-dot {dotcls}"></span>'
                 f'<span class="inc-time">{esc(s)}</span>'
                 f'<span class="inc-text">{esc(text)}{badge}</span>'
                 f'<span class="inc-d">{esc(dur_s)}</span></div>')
    return rows


def render(d, series, overall, h, lang):
    t = STRINGS[lang]
    cz = d["customizations"] or {}
    auto_update_ok = bool(cz.get("ok", False))
    core_ok = d["_core_ok"]
    has_open_bad = any(i.get("open") and i.get("sev") in ("crit", "err") for i in h.get("incidents", []))
    has_open_any = any(i.get("open") for i in h.get("incidents", []))

    if not core_ok or has_open_bad:
        banner, bcls = t["major"], "bad"
    elif not auto_update_ok or has_open_any:
        banner, bcls = t["partial"], "warn"
    else:
        banner, bcls = t["all_ok"], "ok"

    # 人工配置提醒（update.sh 写入），需要用户介入的配置项
    notice = d.get("config_notice")
    notice_html = ""
    if notice and notice.get("msg"):
        notice_html = (f'<div class="banner warn" style="margin-top:12px;font-size:14px;font-weight:600">'
                       f'⚠ {esc(notice["msg"])}</div>')

    fe, be, pro, com, tr, host, dep = (d["frontend"], d["backend"], d["processes"],
                                       d["community"], d["traffic"], d["host"], d["deployed"])
    dep_time_s = datetime.fromtimestamp(dep["time"]).strftime("%m-%d %H:%M") if dep["time"] else "—"

    # dev 环境（独立区块：仅展示，不参与主站横幅/可用率/事件）
    dev = d.get("dev") or {}
    dev_fe = dev.get("frontend") or {}
    dev_be = dev.get("backend") or {}
    dev_pro = dev.get("processes") or {}
    dev_dep = dev.get("deployed") or {}
    dev_up = dev_dep.get("sha")
    dev_exist = bool(dev_up)
    dev_time_s = datetime.fromtimestamp(dev_dep["time"]).strftime("%m-%d %H:%M") if dev_dep.get("time") else "—"
    if dev_exist:
        dev_update_text = (t["pending"].format(sha=dev.get("remote") or "") if dev.get("pending")
                           else t["operational"])
    else:
        dev_update_text = t["unknown"]

    if d["pending"]:
        up_text = t["pending"].format(sha=d["remote"] or "")
        up_ok = True
    else:
        up_text = t["operational"]
        up_ok = True

    def comp_row(name, detail, ok, status_text):
        cls = "ok" if ok else "bad"
        return (f'<div class="comp"><div class="comp-l"><div class="comp-name">{esc(name)}</div>'
                f'<div class="comp-detail">{esc(detail)}</div></div>'
                f'<div class="comp-r {cls}">{esc(status_text)}</div></div>')

    dev_components = (
        comp_row(t["c_dev_website"], t["d_dev_website"].format(ms=dev_fe.get("ms", "—")), bool(dev_fe.get("ok")),
                 t["operational"] if dev_fe.get("ok") else (t["outage"] if dev_exist else t["unknown"]))
        + comp_row(t["c_dev_api"], t["d_dev_api"].format(ms=dev_be.get("ms", "—"),
                    up=fmt_uptime((dev_pro.get("backend") or {}).get("uptime"), lang)),
                   bool(dev_be.get("ok")),
                   t["operational"] if dev_be.get("ok") else (t["outage"] if dev_exist else t["unknown"]))
        + comp_row(t["c_dev_db"], t["d_dev_db"].format(up=fmt_uptime(dev_be.get("uptime_s"), lang)),
                   dev.get("db") == "ok",
                   t["operational"] if dev.get("db") == "ok"
                   else (t["outage"] if dev.get("db") == "error" else t["unknown"]))
        + comp_row(t["c_dev_update"], t["d_dev_update"].format(sha=dev_up or "—", time=dev_time_s),
                   dev_exist and not dev.get("pending"), dev_update_text)
    )

    components = (
        comp_row(t["c_website"], t["d_website"].format(ms=fe["ms"]), fe["ok"],
                 t["operational"] if fe["ok"] else t["outage"])
        + comp_row(t["c_api"], t["d_api"].format(ms=be["ms"], up=fmt_uptime(pro["backend"]["uptime"], lang)),
                   be["ok"], t["operational"] if be["ok"] else t["outage"])
        + comp_row(t["c_db"], t["d_db"].format(up=fmt_uptime(d["backend"].get("uptime_s"), lang)),
                   d["db"] == "ok",
                   t["operational"] if d["db"] == "ok" else (t["outage"] if d["db"] == "error" else t["unknown"]))
        + comp_row(t["c_update"], t["d_update"].format(sha=dep["sha"] or "—", time=dep_time_s),
                   up_ok and auto_update_ok,
                   t["manual"] if not auto_update_ok else up_text)
    )

    bars = ""
    for day, pct in series:
        if pct is None:
            cls, tip = "nodata", f"{day} · {t['nodata']}"
        elif pct >= 99.99:
            cls, tip = "ok", f"{day} · 100%"
        elif pct >= 95:
            cls, tip = "warn", f"{day} · {pct:.1f}%"
        else:
            cls, tip = "bad", f"{day} · {pct:.1f}%"
        bars += f'<span class="bar {cls}" title="{tip}"></span>'
    avail_text = f"{overall:.2f}%" if overall is not None else t["collecting"]

    def metric(v, label):
        return f'<div class="metric"><div class="mv">{esc(v)}</div><div class="ml">{esc(label)}</div></div>'
    metrics = (
        metric(com.get("online", 0), t["m_online"])
        + metric(com.get("users", "—"), t["m_users"])
        + metric(com.get("discussions", "—"), t["m_posts"])
        + metric(com.get("replies", "—"), t["m_replies"])
        + metric(tr.get("rpm", "—"), t["m_requests"])
        + metric((f"{tr['avg_ms']} ms" if tr.get("avg_ms") is not None else "—"), t["m_avg"])
        + metric((f"{pro['backend']['mem_mb']} MB" if pro["backend"]["mem_mb"] else "—"), t["m_bemem"])
        + metric((f"{pro['frontend']['mem_mb']} MB" if pro["frontend"]["mem_mb"] else "—"), t["m_femem"])
        + metric(f"{host['disk_pct']}%", t["m_disk"])
        + metric(f"{host['mem_pct']}%", t["m_mem"])
        + metric(host["load1"], t["m_load"])
        + metric(fmt_uptime(host["host_uptime_s"], lang), t["m_hostup"])
        + metric((str(d["code_report"]["totals"]["errors"]) if d["code_report"] else "—"), t["m_codes"])
        + metric((str(d["code_report"]["totals"]["warnings"]) if d["code_report"] else "—"), t["m_cwarns"])
    )

    if com.get("latest"):
        posts = "".join(
            f'<div class="post"><span class="pt">{esc(p["title"])}</span>'
            f'<span class="pm">{esc(p["author"])} · {t["replies"].format(n=p["replies"])} · {esc(p["at"])}</span></div>'
            for p in com["latest"])
    else:
        posts = f'<div class="empty">{t["empty"]}</div>'

    incidents_html = render_incidents(h, lang)

    rep = d.get("code_report")
    if rep and rep.get("findings"):
        dur = rep.get("duration_ms", 0)
        audit_summary = (str(rep["totals"]["errors"]) + " " + t["t_sev"] + " · "
                         + str(rep["totals"]["warnings"]) + " " + t["m_cwarns"] + " · "
                         + str(round(dur / 1000, 1)) + "s")
        rows_a = ""
        for f in sorted(rep["findings"], key=lambda x: (0 if x["sev"] == "error" else 1, x["file"], x["line"])):
            sevcls = "sev-err" if f["sev"] == "error" else "sev-warn"
            msg = esc(f["message"])
            rows_a += (f'<tr><td class="{sevcls}">{esc(f["sev"])}</td>'
                       f'<td class="loc">{esc(f["file"].split("/")[-1])}:{f["line"]}</td>'
                       f'<td class="rule">{esc(f["rule"])}</td>'
                       f'<td class="msg" title="{msg}">{msg}</td></tr>')
        audit_table = ('<div class="atable"><table><thead><tr><th>' + esc(t["t_sev"]) + '</th><th>'
                       + esc(t["t_loc"]) + '</th><th>' + esc(t["t_rule"]) + '</th><th>'
                       + esc(t["t_msg"]) + '</th></tr></thead><tbody>' + rows_a + '</tbody></table></div>')
    else:
        audit_summary = t["audit_empty"]
        audit_table = f'<div class="empty">{t["audit_empty"]}</div>'

    stale_js = f"""
window.addEventListener('load', function () {{
  var gen = +document.getElementById('generated').getAttribute('data-ts');
  if ((Date.now() - gen) / 1000 > 300) {{
    var w = document.createElement('div');
    w.className = 'banner warn';
    w.textContent = {json.dumps(t["stale"], ensure_ascii=False)};
    document.body.prepend(w);
  }}
  var langs = {json.dumps(LANGS)};
  var path = location.pathname;
  var cur = 'zh';
  if (path.length >= 4 && langs.indexOf(path.slice(1, 3)) >= 0) cur = path.slice(1, 3);
  var want = localStorage.getItem('lang');
  if (!want && !sessionStorage.getItem('langAuto')) {{
    var nav = ((navigator.language || '') + ' ' + (navigator.languages || []).join(' ')).toLowerCase();
    want = 'zh';
    for (var i = 0; i < langs.length; i++) {{ if (nav.indexOf(langs[i]) >= 0) {{ want = langs[i]; break; }} }}
    if (want === 'zh') {{
      try {{
        var tz = Intl.DateTimeFormat().resolvedOptions().timeZone || '';
        var tzmap = {{'Asia/Tokyo': 'ja', 'Europe/Paris': 'fr', 'Europe/Brussels': 'fr',
                     'Europe/Berlin': 'de', 'Europe/Vienna': 'de', 'Europe/Zurich': 'de',
                     'Europe/Madrid': 'es', 'America/Mexico_City': 'es',
                     'America/Argentina/Buenos_Aires': 'es', 'America/Sao_Paulo': 'es'}};
        if (tzmap[tz]) want = tzmap[tz];
      }} catch (e) {{}}
    }}
    sessionStorage.setItem('langAuto', '1');
  }}
  if (want && want !== cur && langs.indexOf(want) >= 0) {{
    location.replace(want === 'zh' ? '/' : '/' + want + '/');
  }}
  var pref = localStorage.getItem('theme') || 'system';
  var mq = window.matchMedia('(prefers-color-scheme: dark)');
  function applyTheme() {{
    var eff = pref === 'system' ? (mq.matches ? 'dark' : 'light') : pref;
    document.documentElement.dataset.theme = eff;
    document.querySelectorAll('#themedd [data-tv]').forEach(function (li) {{
      li.classList.toggle('sel', li.dataset.tv === pref);
    }});
  }}
  document.querySelectorAll('#themedd [data-tv]').forEach(function (li) {{
    li.addEventListener('click', function () {{
      pref = this.dataset.tv;
      localStorage.setItem('theme', pref);
      applyTheme();
      document.getElementById('themedd').classList.remove('open');
    }});
  }});
  if (mq.addEventListener) mq.addEventListener('change', function () {{ if (pref === 'system') applyTheme(); }});
  applyTheme();
  var langdd = document.getElementById('langdd');
  langdd.addEventListener('click', function (e) {{
    var li = e.target.closest('li[data-lang]');
    if (!li) return;
    e.preventDefault();
    var lang = li.dataset.lang;
    try {{ localStorage.setItem('lang', lang); }} catch (err) {{}}
    window.location.assign(lang === 'zh' ? '/' : '/' + lang + '/');
  }});
  var ab = document.getElementById('auditbox');
  if (localStorage.getItem('auditOpen') === '1') ab.open = true;
  ab.addEventListener('toggle', function () {{ localStorage.setItem('auditOpen', ab.open ? '1' : '0'); }});
  document.addEventListener('click', function (e) {{
    var dd = document.getElementById('langdd');
    if (dd && !dd.contains(e.target)) dd.classList.remove('open');
  }});
}});
"""
    head_theme_js = ("(function(){var p=localStorage.getItem('theme')||'system';"
                     "var d=window.matchMedia&&matchMedia('(prefers-color-scheme: dark)').matches;"
                     "document.documentElement.dataset.theme=p==='system'?(d?'dark':'light'):p;})();")

    return f"""<!DOCTYPE html>
<html lang="{lang}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="60">
<title>{esc(t["title"])}</title>
<script>{head_theme_js}</script>
<style>
:root {{ color-scheme: light;
  --bg:#f6f8fa; --text:#1f2328; --muted:#57606a; --faint:#8b949e;
  --panel:#ffffff; --border:#d0d7de; --row:#eaeef2; --nodata:#e5e9ec;
  --ok:#1a9c6b; --warn:#d97706; --bad:#d92d20; }}
[data-theme="dark"] {{ color-scheme: dark;
  --bg:#0d1117; --text:#e6edf3; --muted:#9198a1; --faint:#6e7681;
  --panel:#161b22; --border:#30363d; --row:#21262d; --nodata:#30363d;
  --ok:#2ea043; --warn:#d29922; --bad:#f85149; }}
* {{ box-sizing: border-box; margin: 0; }}
body {{ background:var(--bg); color:var(--text); font-family:system-ui,-apple-system,"Segoe UI","PingFang SC","Hiragino Sans","Microsoft YaHei",sans-serif; padding:32px 16px; }}
.wrap {{ max-width:820px; margin:0 auto; }}
.top {{ display:flex; justify-content:space-between; align-items:center; margin-bottom:14px; }}
.brand {{ font-size:16px; font-weight:700; }}
.top-r {{ display:flex; gap:8px; align-items:center; }}
.themebtn {{ background:none; border:1px solid var(--border); border-radius:8px; padding:4px 9px; cursor:pointer; font-size:13px; color:var(--text); }}
  border:1px solid var(--border); background:var(--panel); border-radius:8px; padding:4px 10px; font-size:12px; }}
.dd {{ position:relative; }}
.ddbtn {{ background:var(--panel); border:1px solid var(--border); color:var(--text); border-radius:8px; padding:4px 10px; cursor:pointer; font-size:12px; }}
.ddbtn .globe {{ margin-right:5px; }}
.ddbtn .caret {{ margin-left:5px; font-size:10px; color:var(--muted); }}
.ddmenu {{ display:none; position:absolute; right:0; top:calc(100% + 6px); background:var(--panel); border:1px solid var(--border); border-radius:10px; list-style:none; padding:6px; margin:0; min-width:150px; box-shadow:0 8px 24px rgba(0,0,0,.15); z-index:10; }}
.dd.open .ddmenu {{ display:block; }}
.ddmenu li {{ font-size:13px; padding:7px 12px; border-radius:7px; }}
.ddmenu li:hover {{ background:var(--row); }}
.ddmenu li.cur {{ color:var(--ok); font-weight:700; }}
.ddmenu a {{ color:var(--text); text-decoration:none; display:block; }}
.banner {{ padding:20px 24px; border-radius:10px; font-size:19px; font-weight:600; color:#fff; }}
.banner.ok {{ background:var(--ok); }}
.banner.warn {{ background:var(--warn); }}
.banner.bad {{ background:var(--bad); }}
h2 {{ font-size:14px; font-weight:600; color:var(--muted); margin:28px 0 10px; letter-spacing:.3px; }}
.panel {{ background:var(--panel); border:1px solid var(--border); border-radius:10px; overflow:hidden; }}
.comp {{ display:flex; justify-content:space-between; align-items:center; padding:14px 18px; border-bottom:1px solid var(--row); gap:12px; }}
.comp:last-child {{ border-bottom:none; }}
.comp-name {{ font-size:15px; font-weight:600; }}
.comp-detail {{ font-size:12px; color:var(--muted); margin-top:3px; }}
.comp-r {{ font-size:14px; font-weight:600; white-space:nowrap; }}
.comp-r.ok {{ color:var(--ok); }}
.comp-r.bad {{ color:var(--bad); }}
.avail-head {{ display:flex; justify-content:space-between; align-items:baseline; }}
.avail-pct {{ font-size:13px; color:var(--ok); font-weight:600; }}
.bars {{ display:flex; gap:2px; margin-top:10px; }}
.bar {{ flex:1; height:28px; border-radius:2px; min-width:2px; }}
.bar.ok {{ background:var(--ok); }}
.bar.warn {{ background:var(--warn); }}
.bar.bad {{ background:var(--bad); }}
.bar.nodata {{ background:var(--nodata); }}
.bar-leg {{ display:flex; justify-content:space-between; font-size:11px; color:var(--faint); margin-top:6px; }}
.metrics {{ display:grid; grid-template-columns:repeat(auto-fill,minmax(150px,1fr)); }}
.metric {{ padding:14px 18px; border-bottom:1px solid var(--row); border-right:1px solid var(--row); }}
.mv {{ font-size:20px; font-weight:700; }}
.ml {{ font-size:12px; color:var(--muted); margin-top:2px; }}
.post {{ padding:10px 18px; border-bottom:1px solid var(--row); display:flex; justify-content:space-between; gap:12px; align-items:baseline; }}
.post:last-child {{ border-bottom:none; }}
.pt {{ font-size:14px; }}
.pm {{ font-size:12px; color:var(--faint); white-space:nowrap; }}
.inc {{ display:flex; align-items:center; gap:10px; padding:12px 18px; border-bottom:1px solid var(--row); font-size:13px; flex-wrap:wrap; }}
.inc:last-child {{ border-bottom:none; }}
.inc-dot {{ width:8px; height:8px; border-radius:50%; flex:none; }}
.inc-dot.bad {{ background:var(--bad); }}
.inc-dot.warn {{ background:var(--warn); }}
.inc-time {{ color:var(--muted); font-variant-numeric:tabular-nums; }}
.inc-text {{ flex:1; min-width:200px; }}
.inc-d {{ color:var(--muted); font-size:12px; }}
.pill {{ display:inline-block; margin-left:8px; padding:1px 8px; border-radius:20px; font-size:11px; background:var(--bad); color:#fff; font-weight:600; }}
.empty {{ padding:16px 18px; font-size:13px; color:var(--faint); }}
.foot {{ text-align:center; font-size:12px; color:var(--faint); margin-top:24px; position:relative; }}
/* 后台入口：右下角极不显眼的纯文字点，只有知情者看得出来 */
.admin-corner {{ position:absolute; right:0; bottom:-1px; color:var(--faint); text-decoration:none; font-size:11px; opacity:.3; padding:2px 6px; line-height:1; }}
.admin-corner:hover {{ opacity:.85; color:var(--text); }}
.iconbtn {{ width:36px; height:36px; display:inline-flex; align-items:center; justify-content:center; background:var(--panel); border:1px solid var(--border); border-radius:10px; cursor:pointer; color:var(--text); position:relative; }}
.iconbtn:hover {{ background:var(--row); }}
.ti {{ transition:transform .2s ease, opacity .2s ease; }}
[data-theme="dark"] .ti-sun {{ opacity:0; transform:rotate(90deg) scale(0); position:absolute; }}
[data-theme="light"] .ti-moon {{ opacity:0; transform:rotate(-90deg) scale(0); position:absolute; }}
.ddmenu li {{ display:flex; align-items:center; gap:9px; cursor:pointer; }}
.ddmenu li.sel {{ color:var(--ok); font-weight:700; }}
.ddmenu li.sel::after {{ content:"✓"; margin-left:auto; font-size:12px; }}
.audit summary {{ padding:13px 18px; cursor:pointer; font-size:13px; color:var(--muted); list-style:none; display:flex; align-items:center; gap:8px; user-select:none; }}
.audit summary::before {{ content:"▸"; transition:transform .15s; }}
.audit[open] summary::before {{ transform:rotate(90deg); }}
.audit summary:hover {{ background:var(--row); }}
.audit table {{ width:100%; border-collapse:collapse; font-size:12px; }}
.audit th {{ text-align:left; padding:7px 14px; color:var(--faint); font-weight:600; border-bottom:1px solid var(--row); background:var(--panel); }}
.audit td {{ padding:6px 14px; border-bottom:1px solid var(--row); vertical-align:top; }}
.audit tr:last-child td {{ border-bottom:none; }}
.audit .sev-err {{ color:var(--warn); font-weight:700; }}
.audit .sev-warn {{ color:var(--warn); font-weight:700; }}
.audit .loc {{ white-space:nowrap; font-variant-numeric:tabular-nums; color:var(--muted); }}
.audit .rule {{ color:var(--muted); white-space:nowrap; }}
.audit .msg {{ max-width:420px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }}
.atable {{ max-height:420px; overflow:auto; }}
</style>
</head>
<body>
<div class="wrap">
  <div class="top">
    <span class="brand">Samryetha</span>
    <div class="top-r">
      <div class="dd" id="themedd">
        <button class="iconbtn" id="themebtn" title="Theme" aria-haspopup="menu"
          onclick="event.stopPropagation();document.getElementById('themedd').classList.toggle('open')">
          {svg("sun", "ti ti-sun")}{svg("moon", "ti ti-moon")}
        </button>
        <ul class="ddmenu">
          <li data-tv="light">{svg("sun")}<span>{t["t_light"]}</span></li>
          <li data-tv="dark">{svg("moon")}<span>{t["t_dark"]}</span></li>
          <li data-tv="system">{svg("monitor")}<span>{t["t_system"]}</span></li>
        </ul>
      </div>
      {lang_switcher(lang)}
    </div>
  </div>
  <div class="banner {bcls}">{esc(banner)}</div>
  {notice_html}

  <h2>{esc(t["h_components"])}</h2>
  <div class="panel">{components}</div>

  <h2>{esc(t["h_dev"])}</h2>
  <div class="panel">{dev_components}</div>

  <h2>{esc(t["h_uptime"])}</h2>
  <div class="panel" style="padding:16px 18px">
    <div class="avail-head"><span style="font-size:13px;color:var(--muted)">{esc(t["daily"])}</span><span class="avail-pct">{esc(avail_text)}</span></div>
    <div class="bars">{bars}</div>
    <div class="bar-leg"><span>{esc(t["days_ago"].format(n=DAYS))}</span><span>{esc(t["today"])}</span></div>
  </div>

  <h2>{esc(t["h_metrics"])}</h2>
  <div class="panel metrics">{metrics}</div>

  <h2>{esc(t["h_audit"])}</h2>
  <details class="panel audit" id="auditbox">
    <summary>{esc(audit_summary)}</summary>
    {audit_table}
  </details>

  <h2>{esc(t["h_community"])}</h2>
  <div class="panel">{posts}</div>

  <h2>{esc(t["h_incidents"])}</h2>
  <div class="panel">{incidents_html}</div>

  <div class="foot">{esc(t["updated_at"])} <span id="generated" data-ts="{int(d['checked_ts']*1000)}">{esc(d['checked_at'])}</span><a class="admin-corner" href="/update" aria-label="admin">&middot;</a></div>
</div>
<script>{stale_js}</script>
</body>
</html>"""


def main():
    d = collect()
    h = load_history()

    # 可用率统计（core 维度）
    today = datetime.now().strftime("%Y-%m-%d")
    day = h["days"].setdefault(today, {"ok": 0, "down": 0})
    day["ok" if d["_core_ok"] else "down"] += 1
    cutoff = (datetime.now() - timedelta(days=DAYS)).strftime("%Y-%m-%d")
    h["days"] = {k: v for k, v in h["days"].items() if k >= cutoff}

    # 检测引擎
    run_detectors(h, d)
    save_history(h)
    series, overall = uptime_series(h)

    os.makedirs(WWW, exist_ok=True)
    for lang in LANGS:
        html = render(d, series, overall, h, lang)
        if lang == "zh":
            out = os.path.join(WWW, "index.html")
        else:
            os.makedirs(os.path.join(WWW, lang), exist_ok=True)
            out = os.path.join(WWW, lang, "index.html")
        tmp = out + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            f.write(html)
        os.replace(tmp, out)

    json_tmp = os.path.join(WWW, "status.json.tmp")
    with open(json_tmp, "w", encoding="utf-8") as f:
        json.dump({k: v for k, v in d.items() if not k.startswith("_")},
                  f, ensure_ascii=False, indent=1, default=str)
    os.replace(json_tmp, os.path.join(WWW, "status.json"))

    open_n = sum(1 for i in h.get("incidents", []) if i.get("open"))
    print(f"[status] {d['checked_at']} core={'OK' if d['_core_ok'] else 'DOWN'} "
          f"online={d['community'].get('online')} rpm={d['traffic'].get('rpm')} "
          f"avail={overall} open_incidents={open_n}")
    if not d["_core_ok"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
