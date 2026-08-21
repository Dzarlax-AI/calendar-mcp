(() => {
  "use strict";

  const pendingSelector = "[data-pending-label]";
  const originalContents = new WeakMap();

  function setPending(control) {
    if (control.getAttribute("aria-busy") === "true") {
      return false;
    }

    originalContents.set(control, Array.from(control.childNodes));
    control.setAttribute("aria-busy", "true");
    control.classList.add("is-pending");

    if (control instanceof HTMLButtonElement) {
      control.disabled = true;
    } else {
      control.setAttribute("aria-disabled", "true");
    }

    const spinner = document.createElement("span");
    spinner.className = "button-spinner";
    spinner.setAttribute("aria-hidden", "true");

    const label = document.createElement("span");
    label.textContent = control.dataset.pendingLabel;
    control.replaceChildren(spinner, label);
    return true;
  }

  function restorePending(control) {
    const contents = originalContents.get(control);
    if (!contents) {
      return;
    }

    control.replaceChildren(...contents);
    control.removeAttribute("aria-busy");
    control.removeAttribute("aria-disabled");
    control.classList.remove("is-pending");
    if (control instanceof HTMLButtonElement) {
      control.disabled = false;
    }
    originalContents.delete(control);
  }

  document.addEventListener("click", (event) => {
    const link = event.target.closest(`a${pendingSelector}`);
    if (!link) {
      return;
    }
    if (link.getAttribute("aria-busy") === "true") {
      event.preventDefault();
      return;
    }
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || link.target === "_blank" || link.hasAttribute("download")) {
      return;
    }
    setPending(link);
  });

  document.addEventListener("submit", (event) => {
    const submitter = event.submitter;
    if (submitter && submitter.matches(pendingSelector)) {
      setPending(submitter);
    }
  });

  window.addEventListener("pageshow", () => {
    document.querySelectorAll(`${pendingSelector}[aria-busy="true"]`).forEach(restorePending);
  });

  const mcpAccess = document.querySelector("[data-mcp-access]");
  if (!mcpAccess) {
    return;
  }

  const revealForm = mcpAccess.querySelector("[data-mcp-reveal-form]");
  const revealButton = mcpAccess.querySelector("[data-mcp-reveal]");
  const copyButton = mcpAccess.querySelector("[data-mcp-copy]");
  const hideButton = mcpAccess.querySelector("[data-mcp-hide]");
  const token = mcpAccess.querySelector("[data-mcp-token]");
  const feedback = mcpAccess.querySelector("[data-mcp-feedback]");
  const maskedToken = token.textContent;
  let revealedToken = "";
  let hideTimer;
  let feedbackTimer;
  let revealGeneration = 0;

  function setFeedback(message, isError = false) {
    window.clearTimeout(feedbackTimer);
    feedback.textContent = message;
    feedback.classList.toggle("error", isError);
  }

  function hideToken() {
    revealGeneration += 1;
    window.clearTimeout(hideTimer);
    revealedToken = "";
    token.textContent = maskedToken;
    token.setAttribute("aria-label", "API key masked");
    copyButton.disabled = true;
    hideButton.disabled = true;
    revealButton.disabled = false;
    setFeedback("");
  }

  revealButton.addEventListener("click", async () => {
    if (!setPending(revealButton)) {
      return;
    }
    const requestGeneration = ++revealGeneration;
    setFeedback("");
    try {
      const response = await fetch(revealForm.action, {
        method: "POST",
        body: new FormData(revealForm),
        credentials: "same-origin",
        headers: { Accept: "application/json" },
      });
      const result = await response.json().catch(() => ({}));
      if (!response.ok || typeof result.token !== "string" || result.token === "") {
        throw new Error(typeof result.error === "string" ? result.error : "The MCP API key could not be revealed.");
      }
      if (requestGeneration !== revealGeneration) {
        return;
      }
      revealedToken = result.token;
      token.textContent = revealedToken;
      token.setAttribute("aria-label", "API key revealed");
      copyButton.disabled = false;
      hideButton.disabled = false;
      window.clearTimeout(hideTimer);
      hideTimer = window.setTimeout(hideToken, 30000);
    } catch (error) {
      hideToken();
      setFeedback(error instanceof Error ? error.message : "The MCP API key could not be revealed.", true);
    } finally {
      restorePending(revealButton);
    }
  });

  copyButton.addEventListener("click", async () => {
    if (revealedToken === "") {
      return;
    }
    try {
      await navigator.clipboard.writeText(revealedToken);
      setFeedback("Copied");
      feedbackTimer = window.setTimeout(() => setFeedback(""), 2000);
    } catch {
      setFeedback("The API key could not be copied. Copy it manually.", true);
    }
  });

  hideButton.addEventListener("click", hideToken);
  window.addEventListener("pagehide", hideToken);
})();
