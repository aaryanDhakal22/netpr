from django.shortcuts import render
from .models import FileModel

def homepage(req):
    return render(req,"job/homepage.html")

def handle_upload(title,file):
    if title=="unknown":
        return
    filename = file.name
    newFile = FileModel(title=title,file=file)
    newFile.save()
    print(filename)


def upload_file(req):
    if req.method == "POST":
        uploaded_file = req.FILES.get('inp-file',False)
        title = req.POST["inp-title"]
        password = req.POST["password"]
        if password!="aaryan150":
            if uploaded_file!= False:
                handle_upload(title,uploaded_file)
            else:
                response = render(req,'job/filenotfound.html')
                response.status_code = 404
                return response
            return render(req,"job/success.html")
        else:
            return render(req,"job/invalid.html")
