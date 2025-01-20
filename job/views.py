from django.shortcuts import render,Http404

#print_job Create your views here.
def homepage(req):
    return render(req,"job/homepage.html")

def handle_upload(file):
    filename = file.name
    print(filename)

def upload_file(req):
    if req.method == "POST":
        uploaded_file = req.FILES.get('inp-file',False)
        if uploaded_file!= False:
            handle_upload(uploaded_file)
        else:
            response = render(req, template_name)
            response.status_code = 404
            return response
        return render(req,"job/success.html")
