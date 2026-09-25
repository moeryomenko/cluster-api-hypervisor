INSERT INTO vms(installation_id, owner_uid, node_id, unit, generation, pid, disk, api_socket, vhost_socket, network, port, mac, ip, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(installation_id, owner_uid)
DO UPDATE SET node_id=excluded.node_id
            , unit=excluded.unit
            , generation=excluded.generation
            , pid=excluded.pid
            , disk=excluded.disk
            , api_socket=excluded.api_socket
            , vhost_socket=excluded.vhost_socket
            , network=excluded.network
            , port=excluded.port
            , mac=excluded.mac
            , ip=excluded.ip
            , updated_at=excluded.updated_at
