SELECT installation_id
     , owner_uid
     , guest_port
     , host_port
  FROM published_ports
 WHERE installation_id=?
   AND owner_uid=?
   AND guest_port=?;
