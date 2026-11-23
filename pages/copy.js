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
  const video = document.getElementById("hero-video");
  const toggle = document.getElementById("hero-motion-toggle");
  if (!video || !toggle) return;

  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");

  function syncButton() {
    const paused = video.paused;
    toggle.setAttribute("aria-pressed", String(paused));
    toggle.textContent = paused ? "Play animation" : "Pause animation";
  }

  function pauseVideo() {
    video.pause();
    syncButton();
  }

  function playVideo() {
    const result = video.play();
    if (result && typeof result.catch === "function") {
      result.catch(() => {});
    }
    syncButton();
  }

  toggle.hidden = false;
  video.addEventListener("play", syncButton);
  video.addEventListener("pause", syncButton);

  if (reducedMotion.matches) {
    video.removeAttribute("autoplay");
    pauseVideo();
  } else {
    syncButton();
  }

  toggle.addEventListener("click", () => {
    if (video.paused) playVideo();
    else pauseVideo();
  });

  reducedMotion.addEventListener("change", (event) => {
    if (event.matches) pauseVideo();
    else playVideo();
  });
})();
