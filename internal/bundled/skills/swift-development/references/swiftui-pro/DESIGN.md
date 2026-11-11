# Design System Document: The Editorial Financial Experience

## 1. Overview & Creative North Star
**Creative North Star: "The Digital Private Vault"**

This design system transcends the transactional nature of finance to create an atmosphere of quiet authority and bespoke craftsmanship. We are moving away from the "utility-first" clutter of traditional fintech. Our goal is to present financial data as a curated editorial experience.

To break the "template" look, we utilize **intentional asymmetry** and **tonal depth**. We prioritize breathing room over information density. By leveraging Apple’s Human Interface Guidelines (HIG) as a structural foundation and layering it with high-end editorial sensibilities, we ensure the UI feels less like a tool and more like a premium service. 

**Key Principles:**
- **Asymmetric Balance:** Aligning core data points to a strong left axis while allowing secondary information to float with generous white space.
- **Tonal Authority:** Using the "Deep Charcoal" palette to create a sense of infinite depth.
- **Micro-Interactions:** Subtle, fluid transitions that mimic the tactile feel of high-end mechanical instruments.

---

## 2. Colors
Our palette is rooted in the depth of `background: #131315`. We do not use "pure" black (#000) for surfaces to avoid "crushing" the shadows; instead, we use varying shades of charcoal to define space.

### The "No-Line" Rule
**Explicit Instruction:** Designers are prohibited from using 1px solid borders to section off content. Boundaries must be defined solely through background color shifts. Use `surface-container-low` for large section backgrounds sitting on the primary `surface`. This creates a seamless, "liquid" interface.

### Surface Hierarchy & Nesting
Treat the UI as a series of physical layers. Hierarchy is achieved through "Tonal Stacking":
1.  **Base Layer:** `surface` (#131315) – The foundation.
2.  **Secondary Sectioning:** `surface-container-low` (#1b1b1d) – Used for grouping related content.
3.  **Interactive Elements:** `surface-container-high` (#2a2a2c) – For elevated interactive cards.
4.  **Prominent Modals:** `surface-container-highest` (#353437) – For the most immediate foreground elements.

### The "Glass & Gradient" Rule
To inject "soul" into the interface:
- **Glassmorphism:** For floating headers or navigation bars, use `surface` with 70% opacity and a `20px` backdrop-blur.
- **Signature Gradients:** Main CTAs should not be flat. Use a linear gradient from `primary` (#aac7ff) to `primary-container` (#3e90ff) at a 135-degree angle to provide a metallic, premium sheen.

---

## 3. Typography
We utilize the Inter typeface (as a proxy for SF Pro) to maintain a clean, modernist aesthetic. The hierarchy is designed to be "Editorial"—meaning large, bold headlines paired with understated, functional body text.

*   **Display (Large/Medium):** Reserved for account balances and high-level summaries. These should feel authoritative.
*   **Headline & Title:** Used for screen titles and section headers. Use `headline-sm` (1.5rem) for a sophisticated "magazine" feel.
*   **Label & Body:** These are functional. Use `label-md` with increased letter-spacing (0.05rem) for a technical, precise look in data tables.

**Hierarchy Note:** Always pair a `display-md` balance with a `label-sm` secondary currency to create a high-contrast visual anchor.

---

## 4. Elevation & Depth
In this system, depth is a result of light and layering, not structural lines.

*   **The Layering Principle:** To create a "lifted" card effect, place a `surface-container-highest` element on top of a `surface-container-low` background. The subtle 3-4% shift in hex value is enough for the human eye to perceive depth without visual noise.
*   **Ambient Shadows:** For floating action buttons or modal sheets, use extra-diffused shadows. 
    *   *Spec:* `Y: 20px, Blur: 40px, Color: #000000 at 12% opacity`.
*   **The "Ghost Border" Fallback:** If a border is required for accessibility (e.g., in high-contrast modes), use `outline-variant` (#414755) at **15% opacity**. It should be felt, not seen.
*   **SF Symbols:** Use "Thin" or "Light" weights for SF Symbols to match the sophisticated typography. Heavy icon weights are strictly forbidden as they disrupt the editorial elegance.

---

## 5. Components

### Buttons
*   **Primary:** Gradient-filled (`primary` to `primary-container`), `xl` (1.5rem) roundedness. No border.
*   **Secondary:** `surface-container-high` background with `on-surface` text.
*   **Tertiary:** Ghost style. `on-surface` text with no background, using `title-sm` weight.

### Input Fields
*   **Styling:** Background must be `surface-container-highest` at 50% opacity with a `10px` backdrop blur. 
*   **State:** Focused state uses a 1px `primary` ghost border (20% opacity) and a subtle glow.

### Cards & Lists
*   **The "No Divider" Rule:** Forbid 1px dividers between list items. Use 16px or 24px of vertical white space from the spacing scale to separate transactions.
*   **Financial Cards:** Use a `surface-container-low` base with a subtle `primary-fixed-dim` inner glow (top edge only, 10% opacity) to mimic the edge of a physical credit card.

### Additional Signature Components
*   **The Trend Micro-Chart:** A sparkline component using `primary` for growth and `error` for loss. No axes or labels; it is a purely visual indicator of momentum.
*   **The "Glass" Tab Bar:** A floating bottom navigation element using the Glassmorphism rule, positioned 24px from the screen bottom for a modern, detached feel.

---

## 6. Do's and Don'ts

### Do
*   **Do** use `display-lg` for single, impactful numbers (e.g., Total Net Worth).
*   **Do** embrace "Empty Space." If a screen only has three elements, let them occupy the space they need rather than forcing them to the top.
*   **Do** use SF Symbols in their "Multicolor" or "Hierarchical" rendering modes to add depth to iconography.

### Don't
*   **Don't** use 100% opaque borders. They create "visual friction" that breaks the premium feel.
*   **Don't** use standard "drop shadows" with small blur radii. They look like "web 2.0" and cheapen the app.
*   **Don't** cram data. If a list is long, use a progressive disclosure pattern (e.g., "See More") to keep the initial editorial view clean.
*   **Don't** use pure white (#FFFFFF) for text. Use `on-surface` (#e4e2e4) to reduce eye strain in Dark Mode.