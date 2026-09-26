INSERT INTO published_ports (installation_id, owner_uid, guest_port, host_port, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(installation_id, owner_uid, guest_port)
DO UPDATE SET host_port=excluded.host_port
            , updated_at=excluded.updated_at;
