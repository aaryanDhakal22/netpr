# Authelia + Traefik setup

netpr has no login of its own. Traefik sends every request to Authelia first
(ForwardAuth), and netpr trusts the `Remote-User` header Authelia returns,
but only when the request reaches netpr from `NETPR_TRUSTED_PROXIES`.

## 1. Traefik middleware (`authelia@file`)

compose.yaml expects a middleware named `authelia` in Traefik's file provider.
It must copy `Remote-User` from Authelia's response; Traefik then overwrites
any `Remote-User` a client sent, so it cannot be spoofed through Traefik.

```yaml
# traefik dynamic config
http:
  middlewares:
    authelia:
      forwardAuth:
        address: http://authelia:9091/api/authz/forward-auth
        trustForwardHeader: true
        authResponseHeaders:
          - Remote-User
          - Remote-Groups
          - Remote-Email
          - Remote-Name
```

## 2. Authelia access rule

`one_factor` lets you in with **either** your password **or** a passkey
(YubiKey). That matches how netpr is used: you tap the YubiKey, and someone
you trust can be given the password.

```yaml
# authelia configuration.yml
access_control:
  rules:
    - domain: print.example.com   # NETPR_HOST
      policy: one_factor

webauthn:
  enable_passkey_login: true      # Authelia 4.39+: sign in with a passkey alone
```

Register the YubiKey once from the Authelia portal (Settings → Two-Factor
Authentication → WebAuthn credentials). Authelia emails a verification code
first, using your existing SMTP notifier.

If you would rather require password **and** YubiKey, use `two_factor`; netpr
needs no change.

## 3. `NETPR_TRUSTED_PROXIES`

This is the address range netpr accepts `Remote-User` from. Narrower is better:

- **Best:** Traefik's container IP as a `/32` if you give Traefik a static IP.
- **Otherwise:** the Traefik network's subnet:
  `docker network inspect traefik -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}'`

With a subnet, any *other* container on that network could reach netpr
directly and claim to be you. Only attach containers you trust to the Traefik
network, or pin Traefik's IP.

## 4. Checking it

- Visiting `https://print.example.com` should show the Authelia login first.
- After login, the page footer reads "Signed in as <your username>".
- A request that bypasses Traefik gets `403 not authenticated`.

## Android app (share sheet)

Open netpr in Chrome on Android → menu → **Install app**. "NetPr" then appears
in the share sheet of any app. Shared files are held on the phone until the
page has loaded, so an expired Authelia session just means logging in first:
the files are not lost.
