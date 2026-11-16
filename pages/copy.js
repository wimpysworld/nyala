// Copy buttons: <button data-copy="id"> copies the text of the element with that id.
document.querySelectorAll("button[data-copy]").forEach((button) => {
  const source = document.getElementById(button.dataset.copy);
  if (!source || !navigator.clipboard) return;
  const status = button.parentElement.querySelector('[role="status"]');
  let timer;

  button.hidden = false;
  button.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(source.textContent.trim());
    } catch {
      return;
    }
    button.toggleAttribute("data-copied", true);
    if (status) status.textContent = "Copied";
    clearTimeout(timer);
    timer = setTimeout(() => {
      button.removeAttribute("data-copied");
      if (status) status.textContent = "";
    }, 2000);
  });
});

// Hero terminal animation: allow pausing and respect prefers-reduced-motion.
(() => {
  const img = document.getElementById("hero-gif");
  const toggle = document.getElementById("hero-motion-toggle");
  if (!img || !toggle) return;

  const animatedSrc = img.getAttribute("src");
  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
  let paused = false;

  function snapshotFrame() {
    if (!img.naturalWidth) return "";
    try {
      const canvas = document.createElement("canvas");
      canvas.width = img.naturalWidth;
      canvas.height = img.naturalHeight;
      const ctx = canvas.getContext("2d");
      if (!ctx) return "";
      ctx.drawImage(img, 0, 0);
      return canvas.toDataURL("image/png");
    } catch {
      return "";
    }
  }

  function setPaused(next) {
    if (next) {
      const stillSrc = snapshotFrame();
      if (!stillSrc) return;
      img.src = stillSrc;
    } else {
      img.src = animatedSrc;
    }
    paused = next;
    toggle.setAttribute("aria-pressed", String(paused));
    toggle.textContent = paused ? "Play animation" : "Pause animation";
  }

  function init() {
    if (!snapshotFrame()) return;
    toggle.hidden = false;
    if (reducedMotion.matches) setPaused(true);
  }

  if (img.complete && img.naturalWidth > 0) init();
  else img.addEventListener("load", init, { once: true });

  toggle.addEventListener("click", () => {
    if (paused) {
      img.addEventListener("load", () => {}, { once: true });
      setPaused(false);
    } else {
      setPaused(true);
    }
  });
  reducedMotion.addEventListener("change", (event) => {
    if (event.matches && !paused) setPaused(true);
    else if (!event.matches && paused) setPaused(false);
  });
})();
