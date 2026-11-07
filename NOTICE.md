# Source note

The plugin's Qoder protocol support is ported from Qoder's own clients and from
existing MIT-licensed Qoder implementations. The Go implementation in `go/` is
independent code, but its wire behaviour follows those sources:

- **COSY request envelope**: the RSA-wrapped AES key, the `Authorization: Bearer
  COSY.<payload>.<signature>` header, the signature input over the payload, the
  wrapped key, the timestamp, the wire body and the request path, and the
  `Cosy-*` header set that names the machine, the client version and the scene.
  This comes from Qoder's desktop client and CLI.
- **Body codec**: the shuffled base64 alphabet and the first/last third swap that
  `agent_chat_generation` applies to its JSON body. Ported from the same clients.
- **Device flow**: the PKCE device sign-in that Qoder's CLI uses, including the
  `device/selectAccounts` page, the `deviceToken/poll` request, the job token
  exchange, and the account and usage endpoints it follows.
- **Supporting references**: the MIT-licensed `qoder` package in
  `magpie-community/plugins`, and CLIProxyAPI's own Qoder support
  (`ufec/CLIProxyAPI`, MIT).

Qoder, Qoder CN and the Qoder logo belong to their owner. This project is not
affiliated with or endorsed by Qoder.

## Logo mapping

- `assets/logo.png` is this plugin's own face in a store: the Qoder mark drawn on the
  OMC sticker the ecosystem's plugins share. It identifies the plugin rather than the
  service, and no sign-in card uses it.
- `assets/qoder-color.svg` is the official Qoder mark on its light tile. It is the
  logo of the `qoder` login entry, and it is the image the official plugin source
  publishes for the store.
- `assets/qoder.svg` is the same mark inverted (light glyph on a dark tile). It is
  the logo of the `qoder-cn` login entry, so the two sign-in cards are
  distinguishable at a glance.

`go/assets/` holds the copies the plugin embeds and serves, and they must stay
byte-identical to the files under `assets/`; the test suite compares them.

Qoder names and logos are used only to identify the service the plugin signs in to.
