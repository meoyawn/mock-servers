;(() => {
  "use strict"

  const tokenBySitekey = new Map([
    ["1x00000000000000000000AA", "turnstile-mock-success-token"],
    ["2x00000000000000000000AB", "turnstile-mock-invalid-token"],
    ["3x00000000000000000000BB", "turnstile-mock-expired-token"],
    ["4x00000000000000000000CC", "turnstile-mock-provider-error-token"],
  ])
  const widgets = new Map()
  let nextWidgetID = 1

  const issueToken = widget => {
    const token =
      tokenBySitekey.get(widget.options.sitekey) ??
      "turnstile-mock-invalid-token"
    queueMicrotask(() => {
      if (widgets.has(widget.id)) {
        widget.options.callback(token)
      }
    })
  }

  window.turnstile = {
    render(container, options) {
      const id = `turnstile-mock-${nextWidgetID}`
      nextWidgetID += 1
      const widget = { container, id, options }
      widgets.set(id, widget)
      container.dataset.turnstileWidgetId = id
      issueToken(widget)
      return id
    },
    remove(id) {
      const widget = widgets.get(id)
      if (widget === undefined) {
        return
      }
      delete widget.container.dataset.turnstileWidgetId
      widgets.delete(id)
    },
    reset(id) {
      const widget = widgets.get(id)
      if (widget !== undefined) {
        issueToken(widget)
      }
    },
  }
})()
