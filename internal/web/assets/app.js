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
})();
