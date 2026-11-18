# VPN setup

Globex uses the GlobalProtect VPN client. Connect to the portal at vpn.globex.example and approve the sign-in with Okta Verify multi-factor authentication. If the client shows error GP-512, the device certificate has expired: remove the VPN profile, reinstall it from the self-service app, and connect again. The VPN is required for access to internal dashboards and the build servers.
