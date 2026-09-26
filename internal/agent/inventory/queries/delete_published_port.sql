DELETE FROM published_ports
 WHERE installation_id=?
   AND owner_uid=?
   AND guest_port=?;
