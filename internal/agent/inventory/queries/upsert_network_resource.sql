INSERT INTO network_resources(installation_id, owner_uid, node_id, network, port, mac, ip, updated_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(installation_id, owner_uid)
DO UPDATE SET node_id=excluded.node_id
            , network=excluded.network
            , port=excluded.port
            , mac=excluded.mac
            , ip=excluded.ip
            , updated_at=excluded.updated_at
