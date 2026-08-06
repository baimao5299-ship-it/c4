import * as React from "react"
import { Button as ButtonPrimitive } from "@base-ui/react/button"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

const buttonVariants = cva(
  "group/button inline-flex shrink-0 items-center justify-center rounded-lg border border-transparent bg-clip-padding text-sm font-medium whitespace-nowrap transition-all outline-none select-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 active:not-aria-[haspopup]:translate-y-px disabled:pointer-events-none disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-3 aria-invalid:ring-destructive/20 [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4 [&_svg]:transition-transform [&_svg]:duration-150 hover:[&_svg]:scale-110",
  {
    variants: {
      variant: {
        default:
          "bg-[linear-gradient(135deg,#6366f1,#8b5cf6)] text-white shadow-[inset_0_1px_0_rgba(255,255,255,0.25),0_4px_14px_rgba(99,102,241,0.35)] backdrop-blur-md hover:brightness-110 active:brightness-95",
        outline:
          "border-indigo-200/60 bg-white/45 shadow-[inset_0_1px_0_rgba(255,255,255,0.7)] backdrop-blur-md hover:border-indigo-300/70 hover:bg-white/65 hover:text-indigo-700 aria-expanded:border-indigo-300/70 aria-expanded:bg-white/65 aria-expanded:text-indigo-700",
        secondary:
          "border-indigo-200/60 bg-white/45 text-secondary-foreground shadow-[inset_0_1px_0_rgba(255,255,255,0.7)] backdrop-blur-md hover:border-indigo-300/70 hover:bg-white/65 aria-expanded:border-indigo-300/70 aria-expanded:bg-white/65 aria-expanded:text-secondary-foreground",
        ghost:
          "hover:bg-white/50 hover:text-indigo-700 aria-expanded:bg-white/50 aria-expanded:text-indigo-700",
        destructive:
          "bg-red-500/15 text-red-600 backdrop-blur-md hover:bg-red-500/25 focus-visible:border-red-400/40 focus-visible:ring-red-500/20",
        link: "text-indigo-600 underline-offset-4 hover:underline",
      },
      size: {
        default:
          "h-8 gap-1.5 px-2.5 has-data-[icon=inline-end]:pr-2 has-data-[icon=inline-start]:pl-2",
        xs: "h-6 gap-1 rounded-[min(var(--radius-md),10px)] px-2 text-xs in-data-[slot=button-group]:rounded-lg has-data-[icon=inline-end]:pr-1.5 has-data-[icon=inline-start]:pl-1.5 [&_svg:not([class*='size-'])]:size-3",
        sm: "h-7 gap-1 rounded-[min(var(--radius-md),12px)] px-2.5 text-[0.8rem] in-data-[slot=button-group]:rounded-lg has-data-[icon=inline-end]:pr-1.5 has-data-[icon=inline-start]:pl-1.5 [&_svg:not([class*='size-'])]:size-3.5",
        lg: "h-9 gap-1.5 px-2.5 has-data-[icon=inline-end]:pr-2 has-data-[icon=inline-start]:pl-2",
        icon: "size-8",
        "icon-xs":
          "size-6 rounded-[min(var(--radius-md),10px)] in-data-[slot=button-group]:rounded-lg [&_svg:not([class*='size-'])]:size-3",
        "icon-sm":
          "size-7 rounded-[min(var(--radius-md),12px)] in-data-[slot=button-group]:rounded-lg",
        "icon-lg": "size-9",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  }
)

const Button = React.forwardRef<
  HTMLButtonElement,
  ButtonPrimitive.Props & VariantProps<typeof buttonVariants>
>(function Button(
  { className, variant = "default", size = "default", ...props },
  ref
) {
  return (
    <ButtonPrimitive
      ref={ref}
      data-slot="button"
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  )
})

Button.displayName = "Button"

export { Button, buttonVariants }
