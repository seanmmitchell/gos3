// GET <base>/servers.json — the preset file (public/servers.json) of the
// standalone server. The standalone build leaves it out of the static
// public assets (nuxt.config.ts): the static handler runs before any
// middleware, so it would serve the presets under any Host. Served from
// here, it passes the Host check of ../middleware/00-allowed-hosts.ts. The
// build puts the file's text in the private runtime config (presetsFile).
import { presetsFileResponse } from '../utils/presets-file'

export default defineEventHandler(event => presetsFileResponse(event, useRuntimeConfig(event).presetsFile || null))
