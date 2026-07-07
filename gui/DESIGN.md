---
name: MicroHosted Technical Design System
colors:
  surface: '#111417'
  surface-dim: '#111417'
  surface-bright: '#37393d'
  surface-container-lowest: '#0b0e11'
  surface-container-low: '#191c1f'
  surface-container: '#1d2023'
  surface-container-high: '#272a2e'
  surface-container-highest: '#323538'
  on-surface: '#e1e2e7'
  on-surface-variant: '#bacbb9'
  inverse-surface: '#e1e2e7'
  inverse-on-surface: '#2e3134'
  outline: '#859585'
  outline-variant: '#3b4a3d'
  surface-tint: '#00e475'
  primary: '#75ff9e'
  on-primary: '#003918'
  primary-container: '#00e676'
  on-primary-container: '#00612e'
  inverse-primary: '#006d35'
  secondary: '#c2c7cf'
  on-secondary: '#2c3137'
  secondary-container: '#42474e'
  on-secondary-container: '#b1b5bd'
  tertiary: '#ffe0b4'
  on-tertiary: '#432c00'
  tertiary-container: '#ffbd45'
  on-tertiary-container: '#714d00'
  error: '#ffb4ab'
  on-error: '#690005'
  error-container: '#93000a'
  on-error-container: '#ffdad6'
  primary-fixed: '#62ff96'
  primary-fixed-dim: '#00e475'
  on-primary-fixed: '#00210b'
  on-primary-fixed-variant: '#005226'
  secondary-fixed: '#dee3eb'
  secondary-fixed-dim: '#c2c7cf'
  on-secondary-fixed: '#171c22'
  on-secondary-fixed-variant: '#42474e'
  tertiary-fixed: '#ffdeac'
  tertiary-fixed-dim: '#ffba38'
  on-tertiary-fixed: '#281900'
  on-tertiary-fixed-variant: '#604100'
  background: '#111417'
  on-background: '#e1e2e7'
  surface-variant: '#323538'
typography:
  headline-lg:
    fontFamily: Inter
    fontSize: 24px
    fontWeight: '600'
    lineHeight: 32px
    letterSpacing: -0.02em
  headline-md:
    fontFamily: Inter
    fontSize: 18px
    fontWeight: '600'
    lineHeight: 24px
  body-md:
    fontFamily: Inter
    fontSize: 14px
    fontWeight: '400'
    lineHeight: 20px
  data-mono:
    fontFamily: JetBrains Mono
    fontSize: 13px
    fontWeight: '500'
    lineHeight: 18px
  data-lg:
    fontFamily: JetBrains Mono
    fontSize: 18px
    fontWeight: '600'
    lineHeight: 24px
  label-xs:
    fontFamily: Inter
    fontSize: 11px
    fontWeight: '700'
    lineHeight: 16px
    letterSpacing: 0.05em
rounded:
  sm: 0.125rem
  DEFAULT: 0.25rem
  md: 0.375rem
  lg: 0.5rem
  xl: 0.75rem
  full: 9999px
spacing:
  unit: 4px
  gutter: 16px
  margin: 24px
  panel-padding: 12px
---

## Brand & Style
The design system is engineered for the high-stakes environment of IoT/OT isolation and security operations. The personality is hyper-functional, precise, and authoritative, evoking the aesthetic of a modern Industrial Control Room. It prioritizes information density and rapid cognitive processing over decorative flair. 

The visual style is a blend of **Corporate Modern** and **Technical Brutalism**:
- **Density:** High-information density allows operators to monitor multiple network segments simultaneously.
- **Visual Language:** Rigid grid structures, surgical precision in alignment, and the use of monospaced fonts for all telemetry data.
- **Emotional Response:** Professionalism, security, and absolute control. The UI acts as a transparent lens for complex hardware-level data.

## Colors
The palette is rooted in a "Lights-Out" dashboard philosophy to reduce eye strain during long shifts while ensuring critical alerts command immediate attention.

- **Foundations:** The primary background is a deep charcoal (#0B0E11), providing a non-distractive canvas. Surfaces use layered grays (#15191E, #1C2127) to define hierarchy without relying on heavy shadows.
- **Accents:** Electric Green (#00E676) is used exclusively for "Healthy" or "Active" states.
- **Alerting:** A strict traffic-light system is implemented. Amber (#FFB300) signifies warnings or non-critical latency; Bright Red (#FF5252) is reserved for quarantine events or hardware failure.
- **Typography:** Headings use pure white for maximum legibility. Secondary data and labels use muted silver to establish a clear visual hierarchy.

## Typography
The typographic system utilizes a dual-font strategy to differentiate between narrative interface elements and technical telemetry.

- **Inter:** Used for the interface shell, navigation, and structural headings. Its humanist-geometric hybrid nature maintains legibility at small scales.
- **JetBrains Mono:** Used for all "living" data, including IP addresses, MAC IDs, log timestamps, and sensor readings. The monospaced nature ensures that columns of numbers align perfectly for easy scanning.
- **Scaling:** Headlines are kept relatively small to accommodate high-density layouts. Mobile views collapse the sidebar into a bottom navigation bar, and `headline-lg` reduces to 20px for handheld monitoring.

## Layout & Spacing
This design system utilizes a **Fixed Grid** approach for the desktop dashboard to ensure consistent alignment of monitoring widgets.

- **Grid:** A 12-column grid with a 16px gutter. Main content areas are typically divided into 3 or 4-column spans for widgets.
- **Density:** A 4px baseline shift is used to maintain a "tight" industrial feel. 
- **Sidebar:** A fixed 240px left sidebar handles network segmentation and navigation.
- **Header:** A 56px top bar is reserved for global host health indicators and system-wide search.
- **Mobile Adaptivity:** On mobile, the 12-column grid collapses to a single column. All interactive targets (buttons/inputs) maintain a minimum 44px height despite the dense visual aesthetic.

## Elevation & Depth
Depth is communicated through **Tonal Layering** rather than traditional shadows to maintain a flat, technical aesthetic.

- **Base:** #0B0E11 (Main background).
- **Surface:** #15191E (Widget containers/Cards).
- **Overlay:** #1C2127 (Modals, tooltips, and hovered states).
- **Borders:** Subtle 1px solid borders using #2D3748 are used to define card boundaries.
- **Focus:** Active elements utilize a 1px inner stroke of the Primary Green color to indicate selection, avoiding the "float" of heavy shadows.

## Shapes
The shape language is strictly geometric and "Low-Radius." 

- **Corners:** A base radius of 2px is applied to all cards, buttons, and input fields. This provides just enough softness to prevent a purely brutalist feel while maintaining an efficient, industrial silhouette.
- **Icons:** Use a 1.5pt stroke weight with sharp or slightly rounded caps. Avoid filled or bubbly icon styles.

## Components
- **Buttons:** Primary buttons are solid Electric Green with black text. Secondary buttons are ghost-style with a #2D3748 border and white text.
- **Monospace Badges:** Status indicators and IDs (e.g., `0x882A`) use a JetBrains Mono font, 11px size, with a subtle background tint of their respective status color (e.g., 10% opacity green for "Active").
- **Cards:** Simple 1px bordered containers. Header sections of cards should have a subtle bottom border to separate titles from telemetry.
- **Inputs:** Dark backgrounds (#0B0E11) with a 1px border. On focus, the border changes to Electric Green.
- **Telemetry Lines:** Sparklines and progress bars should be thin (2px-4px height). Use the Electric Green for normal thresholds and Amber/Red for breach points.
- **Network Tree:** The left sidebar uses a nested list with thin vertical guide lines to show hierarchy between segments and individual gateways.