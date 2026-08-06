import { mergeProps } from "@base-ui/react/merge-props"
import { useRender } from "@base-ui/react/use-render"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

const badgeVariants = cva(
  "group/badge inline-flex h-5 w-fit shrink-0 items-center justify-center gap-1 overflow-hidden rounded-4xl border border-transparent px-2 py-0.5 text-xs font-medium whitespace-nowrap transition-all focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 has-data-[icon=inline-end]:pr-1.5 has-data-[icon=inline-start]:pl-1.5 aria-invalid:border-destructive aria-invalid:ring-destructive/20 [&>svg]:pointer-events-none [&>svg]:size-3!",
  {
    variants: {
      variant: {
        default:
          "bg-[linear-gradient(135deg,#6366f1,#8b5cf6)] text-white shadow-[inset_0_1px_0_rgba(255,255,255,0.25)] backdrop-blur-md [a]:hover:brightness-110",
        secondary:
          "border-indigo-200/60 bg-indigo-100/60 text-indigo-700 shadow-[inset_0_1px_0_rgba(255,255,255,0.6)] backdrop-blur-md [a]:hover:bg-indigo-100/80",
        destructive:
          "bg-red-500/10 text-red-600 focus-visible:ring-red-500/20 [a]:hover:bg-red-500/20",
        outline:
          "border-indigo-300/50 bg-white/40 text-foreground backdrop-blur-md [a]:hover:bg-white/60 [a]:hover:text-indigo-700",
        ghost:
          "hover:bg-white/50 hover:text-indigo-700",
        link: "text-indigo-600 underline-offset-4 hover:underline",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  }
)

function Badge({
  className,
  variant = "default",
  render,
  ...props
}: useRender.ComponentProps<"span"> & VariantProps<typeof badgeVariants>) {
  return useRender({
    defaultTagName: "span",
    props: mergeProps<"span">(
      {
        className: cn(badgeVariants({ variant }), className),
      },
      props
    ),
    render,
    state: {
      slot: "badge",
      variant,
    },
  })
}

export { Badge, badgeVariants }
