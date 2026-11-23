#!/bin/bash
# The RANGER component's Admin server (#2162): configures Ranger's
# install.properties for the cluster's MySQL (on the master, localhost:3306
# in the master's network) and Solr (ranger_audits, localhost:8983), runs
# Ranger's setup.sh once, starts the Admin server on 6080 and stays in the
# foreground while it runs. The passwords are files CloudBurrow mounts at
# /cb/ranger: admin-password (decrypted from
# dataproc:ranger.admin.password.uri with dataproc:ranger.kms.key.uri) and
# db-root-password (the cluster MySQL's root password). Once the Admin
# server answers, it creates the Ranger services a Dataproc cluster's
# plugins use (hadoop-dataproc, hive-dataproc and yarn-dataproc, the names
# Google's "Use Ranger with Kerberos" page shows), CB_MASTER being the
# cluster's master host name.
set -euo pipefail
home=/opt/ranger/admin
python3 -I - "$home/install.properties" <<'PY'
import sys
path = sys.argv[1]
admin = open("/cb/ranger/admin-password").read().strip()
root = open("/cb/ranger/db-root-password").read().strip()
want = {
    "PYTHON_COMMAND_INVOKER": "python3", "DB_FLAVOR": "MYSQL",
    "SQL_CONNECTOR_JAR": "/usr/share/java/mysql-connector.jar",
    "db_root_user": "root", "db_root_password": root, "db_host": "localhost:3306",
    "db_name": "ranger", "db_user": "rangeradmin", "db_password": root,
    "rangerAdmin_password": admin, "rangerTagsync_password": admin,
    "rangerUsersync_password": admin, "keyadmin_password": admin,
    "audit_store": "solr", "audit_solr_urls": "http://localhost:8983/solr/ranger_audits",
    "audit_solr_collection_name": "ranger_audits",
    "policymgr_external_url": "http://localhost:6080", "policymgr_http_enabled": "true",
    "unix_user": "root", "unix_group": "root", "authentication_method": "NONE",
    "RANGER_ADMIN_LOG_DIR": "/var/log/ranger", "RANGER_PID_DIR_PATH": "/var/run/ranger",
    "ranger_admin_max_heap_size": "1g",
}
out, seen = [], set()
for line in open(path):
    k = line.split("=", 1)[0].strip()
    if not line.lstrip().startswith("#") and "=" in line and k in want:
        out.append("%s=%s\n" % (k, want[k]))
        seen.add(k)
    else:
        out.append(line)
for k in want:
    if k not in seen:
        out.append("%s=%s\n" % (k, want[k]))
open(path, "w").write("".join(out))
PY
until (exec 3<>/dev/tcp/127.0.0.1/3306) 2>/dev/null; do sleep 2; done
cd "$home"
if [ ! -e /var/lib/cloudburrow-ranger-setup-done ]; then
  ./setup.sh
  touch /var/lib/cloudburrow-ranger-setup-done
fi
./ews/ranger-admin-services.sh start
until python3 -I -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:6080/login.jsp", timeout=5)' 2>/dev/null; do sleep 3; done
python3 -I - <<'PY'
import base64, json, os, urllib.error, urllib.request
admin = open("/cb/ranger/admin-password").read().strip()
auth = "Basic " + base64.b64encode(("admin:" + admin).encode()).decode()
master = os.environ.get("CB_MASTER", "localhost")
services = [
    {"name": "hadoop-dataproc", "type": "hdfs", "configs": {"username": "hdfs", "password": admin,
        "fs.default.name": "hdfs://%s" % master, "hadoop.security.authorization": "true",
        "hadoop.security.authentication": "simple"}},
    {"name": "hive-dataproc", "type": "hive", "configs": {"username": "hive", "password": admin,
        "jdbc.driverClassName": "org.apache.hive.jdbc.HiveDriver", "jdbc.url": "jdbc:hive2://%s:10000" % master}},
    {"name": "yarn-dataproc", "type": "yarn", "configs": {"username": "yarn", "password": admin,
        "yarn.url": "http://%s:8088" % master, "hadoop.security.authentication": "simple"}},
]
# The master's users, as Ranger's Unix usersync would add them on a
# Dataproc master (Ranger refuses a policy naming a user it lacks): the
# users jobs and daemons run as.
import secrets
for user in ["root", "hive", "hdfs", "yarn", "mapred", "spark"]:
    req = urllib.request.Request("http://127.0.0.1:6080/service/xusers/users/userName/" + user,
        headers={"Authorization": auth, "Accept": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=30)
        continue
    except urllib.error.HTTPError as e:
        if e.code not in (400, 404):
            raise
    body = {"name": user, "firstName": user, "password": "Cb1" + secrets.token_hex(12), "userSource": 1,
        "status": 1, "isVisible": 1, "userRoleList": ["ROLE_USER"], "description": "Unix user of the cluster's master"}
    req = urllib.request.Request("http://127.0.0.1:6080/service/xusers/secure/users", data=json.dumps(body).encode(), method="POST",
        headers={"Authorization": auth, "Accept": "application/json", "Content-Type": "application/json"})
    urllib.request.urlopen(req, timeout=60)
    print("created Ranger user", user)
base = "http://127.0.0.1:6080/service/public/v2/api/service"
for svc in services:
    req = urllib.request.Request(base + "/name/" + svc["name"], headers={"Authorization": auth, "Accept": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=30)
        continue
    except urllib.error.HTTPError as e:
        if e.code != 404:
            raise
    svc["isEnabled"] = True
    req = urllib.request.Request(base, data=json.dumps(svc).encode(), method="POST",
        headers={"Authorization": auth, "Accept": "application/json", "Content-Type": "application/json"})
    urllib.request.urlopen(req, timeout=60)
    print("created Ranger service", svc["name"])
PY
echo "CLOUDBURROW RANGER ADMIN READY on 6080"
pid=$(pgrep -f org.apache.ranger.server.tomcat.EmbeddedServer)
tail --pid="$pid" -F /var/log/ranger/ranger-admin-*.log
