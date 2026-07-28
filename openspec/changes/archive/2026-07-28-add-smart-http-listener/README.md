# add-smart-http-listener

Add a smart HTTP routing listener on a separate port that applies the same routing rules as the PAC (blacklist/whitelist/local/RU-geo) with Direct/Upstream strategy selection, for clients that ignore the system PAC (notably zcode). Reuses the existing upstream cascade; the dumb HTTP listener stays untouched.
